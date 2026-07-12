// CGO helper layer: compression entry points backed by the real zlib, plus
// comparison utilities.
//
// GZIP frame format (byte-identical to gzip.Writer / zlib.CompressOpts /
// native/gzip_ref.cpp):
//   [1f 8b 08 00 <mtime LE:4> 00 <os>][raw deflate][crc32 LE:4][isize LE:4]
//
// All functions that return a buffer use the format
// [len:4 bytes LE][data:len bytes]; the caller frees it. NULL is returned
// on failure.

#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <zlib.h>

// Internal: write the GZIP header (10 bytes, XFL=0) + trailer (8 bytes),
// returning [len:4][gzip data]
static uint8_t* frame_gzip(uint8_t* buf, const uint8_t* input, size_t in_len,
                           size_t def_len, uint32_t ts, uint8_t os) {
    uint8_t* gz = buf + 4;
    gz[0] = 0x1f; gz[1] = 0x8b;
    gz[2] = 0x08; gz[3] = 0x00;
    gz[4] = ts; gz[5] = ts >> 8; gz[6] = ts >> 16; gz[7] = ts >> 24;
    gz[8] = 0x00; gz[9] = os;

    uint32_t crc = crc32(0, input, in_len);
    size_t t = 10 + def_len;
    gz[t] = crc; gz[t+1] = crc >> 8; gz[t+2] = crc >> 16; gz[t+3] = crc >> 24;
    gz[t+4] = in_len; gz[t+5] = in_len >> 8;
    gz[t+6] = in_len >> 16; gz[t+7] = in_len >> 24;

    uint32_t out_len = 10 + def_len + 8;
    buf[0] = out_len; buf[1] = out_len >> 8;
    buf[2] = out_len >> 16; buf[3] = out_len >> 24;
    return buf;
}

/* ------------------------------------------------------------------
 * Reusable raw deflate stream handle (main compression path)
 *
 * Calling deflateInit2/deflateEnd on every call makes the ~262KB of
 * compression state get allocated/freed repeatedly at the top of the
 * glibc heap; after a heap trim the next init must fault the pages back
 * in, costing ~40µs of allocator overhead on small/medium inputs (and
 * fluctuating bimodally with per-thread arena state). Matching the pure
 * Go side's sync.Pool, the cgo side also reuses streams via deflateReset.
 * ------------------------------------------------------------------ */

// Create a long-lived raw deflate stream (windowBits=-15, memLevel=8,
// default strategy). Returns NULL on failure.
void* deflate_stream_new(int level) {
    z_stream* s = calloc(1, sizeof(z_stream));
    if (!s) return NULL;
    if (deflateInit2(s, level, Z_DEFLATED, -15, 8, Z_DEFAULT_STRATEGY) != Z_OK) {
        free(s);
        return NULL;
    }
    return s;
}

void deflate_stream_free(void* h) {
    z_stream* s = (z_stream*)h;
    if (!s) return;
    deflateEnd(s);
    free(s);
}

// Run one complete raw deflate on an existing stream (deflateReset first).
// Output is byte-identical to compressing with a fresh stream
// (deflateReset is equivalent to a brand-new stream).
uint8_t* deflate_stream_oneshot(void* h, const uint8_t* input, size_t in_len) {
    z_stream* s = (z_stream*)h;
    if (deflateReset(s) != Z_OK) {
        return NULL;
    }

    size_t max = deflateBound(s, in_len);
    uint8_t* buf = malloc(4 + max);
    if (!buf) {
        return NULL;
    }

    s->next_in = (uint8_t*)input;
    s->avail_in = in_len;
    s->next_out = buf + 4;
    s->avail_out = max;

    int r;
    do {
        r = deflate(s, Z_FINISH);
    } while (r == Z_OK); /* level 0 may need a second call */
    uint32_t out_len = (uint32_t)(max - s->avail_out);

    /* Clear the cursors pointing into caller (Go) memory: the stream is
       pooled for reuse and must not retain Go pointers after the call
       returns (cgo pointer rules) */
    s->next_in = NULL;
    s->avail_in = 0;
    s->next_out = NULL;
    s->avail_out = 0;

    if (r != Z_STREAM_END) {
        free(buf);
        return NULL;
    }

    buf[0] = out_len; buf[1] = out_len >> 8;
    buf[2] = out_len >> 16; buf[3] = out_len >> 24;
    return buf;
}

/* ------------------------------------------------------------------
 * The entry points below are for tests/comparison only
 * ------------------------------------------------------------------ */

// Demo only: call Z_SYNC_FLUSH once at split, then Z_FINISH.
// Shows how a sync flush changes the compressed bytes (while the
// decompressed result stays the same).
uint8_t* gzip_compress_sync(const uint8_t* input, size_t in_len,
                            uint32_t ts, int level, uint8_t os, size_t split) {
    if (split > in_len) split = in_len;

    z_stream s = {0};
    if (deflateInit2(&s, level, Z_DEFLATED, -15, 8, Z_DEFAULT_STRATEGY) != Z_OK) {
        return NULL;
    }

    /* A sync flush inserts an empty stored block and disturbs block
       splitting; reserve some headroom */
    size_t max = deflateBound(&s, in_len) + 64;
    uint8_t* buf = malloc(4 + 10 + max + 8);
    if (!buf) {
        deflateEnd(&s);
        return NULL;
    }

    s.next_in = (uint8_t*)input;
    s.avail_in = split;
    s.next_out = buf + 4 + 10;
    s.avail_out = max;

    if (deflate(&s, Z_SYNC_FLUSH) != Z_OK) {
        free(buf);
        deflateEnd(&s);
        return NULL;
    }

    s.avail_in = in_len - split; /* next_in was already advanced by deflate */
    int r;
    do {
        r = deflate(&s, Z_FINISH);
    } while (r == Z_OK);
    if (r != Z_STREAM_END) {
        free(buf);
        deflateEnd(&s);
        return NULL;
    }
    size_t def_len = max - s.avail_out;
    deflateEnd(&s);
    return frame_gzip(buf, input, in_len, def_len, ts, os);
}

// Test only: have C zlib itself produce a complete GZIP stream
// (windowBits=15+16), writing optional header fields via deflateSetHeader
// (extra/name/comment; NULL means omit the corresponding field). Used for
// byte-for-byte comparison against Go gzip.Writer's header output.
// Note: on this path zlib computes XFL from the level (9→2, <2→4,
// otherwise→0).
uint8_t* gzip_compress_header(const uint8_t* input, size_t in_len,
                              int level, uint32_t ts, uint8_t os,
                              const uint8_t* extra, uint32_t extra_len,
                              const char* name, const char* comment) {
    z_stream s = {0};
    if (deflateInit2(&s, level, Z_DEFLATED, 15 + 16, 8, Z_DEFAULT_STRATEGY) != Z_OK) {
        return NULL;
    }

    gz_header h;
    memset(&h, 0, sizeof(h));
    h.time = ts;
    h.os = os;
    h.extra = (Bytef*)extra;
    h.extra_len = extra_len;
    h.name = (Bytef*)name;
    h.comment = (Bytef*)comment;
    if (deflateSetHeader(&s, &h) != Z_OK) {
        deflateEnd(&s);
        return NULL;
    }

    /* deflateBound is called after setHeader, so it already accounts for
       the header fields and the gzip trailer */
    size_t max = deflateBound(&s, in_len) + 64;
    uint8_t* buf = malloc(4 + max);
    if (!buf) {
        deflateEnd(&s);
        return NULL;
    }

    s.next_in = (uint8_t*)input;
    s.avail_in = in_len;
    s.next_out = buf + 4;
    s.avail_out = max;

    int r;
    do {
        r = deflate(&s, Z_FINISH);
    } while (r == Z_OK);
    if (r != Z_STREAM_END) {
        free(buf);
        deflateEnd(&s);
        return NULL;
    }
    uint32_t out_len = (uint32_t)(max - s.avail_out);
    deflateEnd(&s);

    buf[0] = out_len; buf[1] = out_len >> 8;
    buf[2] = out_len >> 16; buf[3] = out_len >> 24;
    return buf;
}

// Test only: streaming raw deflate following the call sequence
// (chunk_lens[i], flushes[i]). flushes take zlib constants
// (0=NO_FLUSH 1=PARTIAL 2=SYNC 3=FULL 4=FINISH). Used for byte-for-byte
// comparison against the pure Go streaming implementation.
// Returns [len:4][data:len].
uint8_t* deflate_raw_stream(const uint8_t* input, size_t in_len, int level,
                            const uint32_t* chunk_lens, const int32_t* flushes,
                            int32_t n_ops) {
    z_stream s = {0};
    if (deflateInit2(&s, level, Z_DEFLATED, -15, 8, Z_DEFAULT_STRATEGY) != Z_OK) {
        return NULL;
    }

    /* Flushes split blocks and insert markers; reserve headroom per op */
    size_t max = deflateBound(&s, in_len) + (size_t)n_ops * 40 + 1024;
    uint8_t* buf = malloc(4 + max);
    if (!buf) {
        deflateEnd(&s);
        return NULL;
    }

    s.next_out = buf + 4;
    s.avail_out = max;

    size_t off = 0;
    for (int32_t i = 0; i < n_ops; i++) {
        size_t len = chunk_lens[i];
        if (off + len > in_len) len = in_len - off;
        s.next_in = (uint8_t*)input + off;
        s.avail_in = len;
        off += len;

        int flush = flushes[i];
        int r;
        do {
            r = deflate(&s, flush);
        } while (r == Z_OK && (s.avail_in > 0 || flush == Z_FINISH));
        /* A redundant flush with nothing to do returns Z_BUF_ERROR;
           treat it as success with no output */
        if (r != Z_OK && r != Z_STREAM_END && r != Z_BUF_ERROR) {
            free(buf);
            deflateEnd(&s);
            return NULL;
        }
    }

    uint32_t out_len = (uint32_t)(max - s.avail_out);
    deflateEnd(&s);

    buf[0] = out_len; buf[1] = out_len >> 8;
    buf[2] = out_len >> 16; buf[3] = out_len >> 24;
    return buf;
}
