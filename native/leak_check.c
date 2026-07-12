// leak_check — drives every entry point of internal/czlib/gzip.c directly;
// combined with -fsanitize=address,leak it gives CI a precise memory
// leak/out-of-bounds safety net for the C layer (LeakSanitizer reports all
// unfreed allocations at process exit).
//
// Build (ubuntu CI):
//   cc -O1 -g -fsanitize=address,leak \
//      native/leak_check.c -Iinternal/czlib/zlib -o leak_check
// Run: ./leak_check   (exits non-zero and prints a report on leak/overflow)

#include <assert.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

#include "../internal/czlib/zlib/zlib_amalgam.c"
#include "../internal/czlib/gzip.c"

int main(void) {
    const size_t n = 300000;
    uint8_t* data = malloc(n);
    assert(data);
    for (size_t i = 0; i < n; i++) data[i] = (uint8_t)((i * 131) % 251);

    for (int level = -1; level <= 9; level++) {
        /* Z_SYNC_FLUSH path */
        uint8_t* r = gzip_compress_sync(data, n, 123456789u, level, 3, n / 2);
        assert(r);
        free(r);

        /* deflateSetHeader path (all header fields) */
        r = gzip_compress_header(data, n, level, 42u, 3,
                                 (const uint8_t*)"\x01\x02", 2,
                                 "name.bin", "a comment");
        assert(r);
        free(r);

        /* reused-stream path (the main compression path) */
        void* h = deflate_stream_new(level == -1 ? 6 : level);
        assert(h);
        for (int i = 0; i < 5; i++) {
            uint8_t* o = deflate_stream_oneshot(h, data, n);
            assert(o);
            free(o);
        }
        deflate_stream_free(h);
    }

    /* streaming call-sequence path */
    uint32_t chunks[3] = {100000, 100000, 100000};
    int32_t flushes[3] = {0 /*NO_FLUSH*/, 2 /*SYNC*/, 4 /*FINISH*/};
    uint8_t* r = deflate_raw_stream(data, n, 6, chunks, flushes, 3);
    assert(r);
    free(r);

    /* deflateSetHeader with randomized parameters (deterministic LCG):
       name/comment length edges with high Latin-1 bytes, extra
       absent / empty-present / up to the 65535 boundary, MTIME and OS
       extremes, all levels — the C header path must stay leak-free and
       in-bounds for every parameter shape */
    {
        static const int str_lens[4] = {0, 1, 255, 1024};
        static const uint32_t mtimes[4] = {0u, 1u, 0x7fffffffu, 0xffffffffu};
        static const int oses[4] = {0, 3, 11, 255};
        static uint8_t extra_buf[65535];
        static char name_buf[1025], comment_buf[1025];
        uint32_t rs = 0x13572468u;
#define LC_NEXT() (rs = rs * 1664525u + 1013904223u)
        for (size_t i = 0; i < sizeof(extra_buf); i++) {
            extra_buf[i] = (uint8_t)LC_NEXT();
        }
        for (int i = 0; i < 60; i++) {
            int name_len = str_lens[LC_NEXT() % 4];
            int comment_len = str_lens[LC_NEXT() % 4];
            for (int j = 0; j < name_len; j++) name_buf[j] = (char)(1 + LC_NEXT() % 255);
            name_buf[name_len] = 0;
            for (int j = 0; j < comment_len; j++) comment_buf[j] = (char)(1 + LC_NEXT() % 255);
            comment_buf[comment_len] = 0;

            int extra_mode = (int)(LC_NEXT() % 3); /* 0: absent, 1: empty-present, 2: random */
            uint32_t extra_len = extra_mode == 2 ? LC_NEXT() % 65536u : 0u;
            const uint8_t* extra = extra_mode ? extra_buf : NULL;

            uint32_t mtime = mtimes[LC_NEXT() % 4];
            int osb = oses[LC_NEXT() % 4];
            int level = (int)(LC_NEXT() % 11) - 1;
            size_t len = 1000 + LC_NEXT() % 20000;

            uint8_t* o = gzip_compress_header(data, len, level, mtime, (uint8_t)osb,
                                              extra, extra_len,
                                              name_len ? name_buf : NULL,
                                              comment_len ? comment_buf : NULL);
            assert(o);
            free(o);
        }
#undef LC_NEXT
    }

    /* empty input and failure paths must not leak */
    r = gzip_compress_sync(data, 0, 0u, 6, 3, 0);
    assert(r);
    free(r);
    assert(deflate_stream_new(99) == NULL); /* invalid level: failed init must free the handle */

    free(data);
    printf("leak_check: OK\n");
    return 0;
}
