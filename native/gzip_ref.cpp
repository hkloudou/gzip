// gzip_ref — standalone C++ native reference tool (this repository's only
// C++ source; it contains no compression logic of its own).
//
// Produces GZIP/deflate byte streams with real zlib — built either against
// the official zlib 1.3.1 tarball (pinned SHA-256; the deterministic
// reference) or the system zlib — acting as the referee for the cross-check:
// its output and the pure Go library's output must match byte-for-byte.
//
// Build (official tarball; `make native-build` automates download + build):
//   (cd zlib-1.3.1 && ./configure --static && make libz.a)
//   c++ -O2 -std=c++17 native/gzip_ref.cpp zlib-1.3.1/libz.a -Izlib-1.3.1 -o gzip_ref
//
// Build (system zlib):
//   c++ -O2 -std=c++17 native/gzip_ref.cpp -lz -o gzip_ref_system
//
// Usage (input on stdin, compressed bytes on stdout):
//   gzip_ref compress <level> <mtime> <os> [flushAt]
//       raw deflate (windowBits=-15, memLevel=8) plus a hand-written 10-byte
//       header (XFL=0) and 8-byte trailer — byte-identical to this library's
//       gzip.Writer framing; optional flushAt: Z_SYNC_FLUSH after the first
//       flushAt bytes, then finish.
//   gzip_ref stream <level> <flush>@<len> [<flush>@<len> ...]
//       raw deflate (windowBits=-15) driven by an explicit call sequence:
//       for each op, the next <len> input bytes are passed to
//       deflate(strm, <flush>) with ample output space — the exact call
//       semantics of this library's streaming Deflater. <flush> uses zlib
//       values (0=NO_FLUSH 1=PARTIAL 2=SYNC 3=FULL 4=FINISH). The sequence
//       must consume the whole input; end with 4@<len> for a complete stream.
//   gzip_ref header <level> <mtime> <os> <nameHex> <commentHex> <extraHex>
//       full GZIP stream produced by zlib itself (windowBits=15+16 +
//       deflateSetHeader). The three fields are passed as hex; "-" means
//       absent.
//       Note: on this path zlib computes XFL per level (9→2, <2→4, else→0).
//   gzip_ref bench <level> <iters>
//       deflateInit2 once, deflateReset per iteration to reuse the stream,
//       performing a full compression each time (reset+deflate+crc+framing,
//       matching the pooled reuse of both Go sides); prints one JSON line to
//       stdout.
//   gzip_ref version
//       prints zlibVersion().
//
// Note: this tool is a test reference implementation; input goes through
// zlib's 32-bit avail_in, inputs above ~3.75GiB are rejected (die), no
// chunking.

#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <chrono>
#include <climits>
#include <string>
#include <vector>

#include <unistd.h>
#include <zlib.h>

namespace {

[[noreturn]] void die(const char* msg) {
    fprintf(stderr, "gzip_ref: %s\n", msg);
    exit(1);
}

std::vector<uint8_t> read_all_stdin() {
    std::vector<uint8_t> buf;
    uint8_t tmp[1 << 16];
    ssize_t n;
    while ((n = read(0, tmp, sizeof(tmp))) > 0) {
        buf.insert(buf.end(), tmp, tmp + n);
        // zlib's avail_in/avail_out are 32-bit; leave headroom for
        // deflateBound expansion, and reject over-limit input outright
        // instead of silently truncating
        if (buf.size() > 0xF0000000ull) {
            die("input too large (32-bit zlib avail_in; this is a test tool)");
        }
    }
    if (n < 0) {
        perror("gzip_ref: read stdin");
        exit(2);
    }
    return buf;
}

void write_all_stdout(const uint8_t* p, size_t n) {
    if (fwrite(p, 1, n, stdout) != n) {
        perror("gzip_ref: write stdout");
        exit(2);
    }
}

int parse_int(const char* s) {
    char* end = nullptr;
    long v = strtol(s, &end, 10);
    if (end == s || *end != '\0') die("invalid integer argument");
    if (v < INT_MIN || v > INT_MAX) die("integer argument out of range");
    return static_cast<int>(v);
}

std::vector<uint8_t> parse_hex(const char* s) {
    std::vector<uint8_t> out;
    size_t len = strlen(s);
    if (len % 2 != 0) die("hex argument must have even length");
    for (size_t i = 0; i < len; i += 2) {
        auto nib = [](char c) -> int {
            if (c >= '0' && c <= '9') return c - '0';
            if (c >= 'a' && c <= 'f') return c - 'a' + 10;
            if (c >= 'A' && c <= 'F') return c - 'A' + 10;
            die("invalid hex digit");
        };
        out.push_back(static_cast<uint8_t>(nib(s[i]) << 4 | nib(s[i + 1])));
    }
    return out;
}

// Framing byte-identical to this library's gzip.Writer /
// internal/czlib/gzip.c frame_gzip:
// [1f 8b 08 00 <mtime LE> 00 <os>][raw deflate][crc32 LE][isize LE]
std::vector<uint8_t> compress_framed(const std::vector<uint8_t>& in,
                                     int level, uint32_t mtime, uint8_t os,
                                     long flush_at /* <0 = none */) {
    z_stream s;
    memset(&s, 0, sizeof(s));
    if (deflateInit2(&s, level, Z_DEFLATED, -15, 8, Z_DEFAULT_STRATEGY) != Z_OK) {
        die("deflateInit2 failed");
    }

    size_t max = deflateBound(&s, in.size()) + 64; // headroom for sync flush
    std::vector<uint8_t> out(10 + max + 8);
    out[0] = 0x1f; out[1] = 0x8b; out[2] = 0x08; out[3] = 0x00;
    out[4] = static_cast<uint8_t>(mtime);
    out[5] = static_cast<uint8_t>(mtime >> 8);
    out[6] = static_cast<uint8_t>(mtime >> 16);
    out[7] = static_cast<uint8_t>(mtime >> 24);
    out[8] = 0x00; // XFL=0, matching this library
    out[9] = os;

    s.next_out = out.data() + 10;
    s.avail_out = static_cast<uInt>(max);

    if (flush_at >= 0) {
        size_t split = static_cast<size_t>(flush_at);
        if (split > in.size()) split = in.size();
        s.next_in = const_cast<Bytef*>(in.data());
        s.avail_in = static_cast<uInt>(split);
        if (deflate(&s, Z_SYNC_FLUSH) != Z_OK) die("deflate(Z_SYNC_FLUSH) failed");
        s.avail_in = static_cast<uInt>(in.size() - split);
    } else {
        s.next_in = const_cast<Bytef*>(in.data());
        s.avail_in = static_cast<uInt>(in.size());
    }

    int r;
    do {
        r = deflate(&s, Z_FINISH);
    } while (r == Z_OK); // level 0 may need one more call
    if (r != Z_STREAM_END) die("deflate(Z_FINISH) failed");

    size_t def_len = max - s.avail_out;
    deflateEnd(&s);

    uint32_t crc = static_cast<uint32_t>(
        crc32(0L, in.empty() ? Z_NULL : in.data(), static_cast<uInt>(in.size())));
    uint32_t isize = static_cast<uint32_t>(in.size());
    size_t t = 10 + def_len;
    out[t]     = static_cast<uint8_t>(crc);
    out[t + 1] = static_cast<uint8_t>(crc >> 8);
    out[t + 2] = static_cast<uint8_t>(crc >> 16);
    out[t + 3] = static_cast<uint8_t>(crc >> 24);
    out[t + 4] = static_cast<uint8_t>(isize);
    out[t + 5] = static_cast<uint8_t>(isize >> 8);
    out[t + 6] = static_cast<uint8_t>(isize >> 16);
    out[t + 7] = static_cast<uint8_t>(isize >> 24);
    out.resize(t + 8);
    return out;
}

// Full GZIP stream produced by zlib itself (windowBits=15+16 +
// deflateSetHeader).
std::vector<uint8_t> compress_header(const std::vector<uint8_t>& in, int level,
                                     uint32_t mtime, uint8_t os,
                                     const std::vector<uint8_t>* name,
                                     const std::vector<uint8_t>* comment,
                                     const std::vector<uint8_t>* extra) {
    z_stream s;
    memset(&s, 0, sizeof(s));
    if (deflateInit2(&s, level, Z_DEFLATED, 15 + 16, 8, Z_DEFAULT_STRATEGY) != Z_OK) {
        die("deflateInit2(gzip) failed");
    }

    // name/comment must be NUL-terminated
    std::vector<uint8_t> name_z, comment_z;
    gz_header h;
    memset(&h, 0, sizeof(h));
    h.time = mtime;
    h.os = os;
    if (name) {
        name_z = *name;
        name_z.push_back(0);
        h.name = name_z.data();
    }
    if (comment) {
        comment_z = *comment;
        comment_z.push_back(0);
        h.comment = comment_z.data();
    }
    if (extra) {
        // An empty-but-present extra must still emit FEXTRA (xlen=0) — zlib
        // treats a non-NULL pointer as "field present", and data() of an
        // empty vector may be nullptr, so provide a fallback
        static Bytef empty_extra[1] = {0};
        h.extra = extra->empty() ? empty_extra : const_cast<Bytef*>(extra->data());
        h.extra_len = static_cast<uInt>(extra->size());
    }
    if (deflateSetHeader(&s, &h) != Z_OK) die("deflateSetHeader failed");

    size_t max = deflateBound(&s, in.size()) + 64;
    std::vector<uint8_t> out(max);
    s.next_in = const_cast<Bytef*>(in.data());
    s.avail_in = static_cast<uInt>(in.size());
    s.next_out = out.data();
    s.avail_out = static_cast<uInt>(max);

    int r;
    do {
        r = deflate(&s, Z_FINISH);
    } while (r == Z_OK);
    if (r != Z_STREAM_END) die("deflate(Z_FINISH) failed");
    out.resize(max - s.avail_out);
    deflateEnd(&s);
    return out;
}

int cmd_compress(int argc, char** argv) {
    if (argc < 5 || argc > 6) die("usage: compress <level> <mtime> <os> [flushAt]");
    int level = parse_int(argv[2]);
    uint32_t mtime = static_cast<uint32_t>(strtoul(argv[3], nullptr, 10));
    uint8_t os = static_cast<uint8_t>(parse_int(argv[4]));
    long flush_at = -1; // internal sentinel: no flush
    if (argc == 6) {
        flush_at = parse_int(argv[5]);
        // An explicitly given flushAt must be non-negative (the Go side
        // clamps negative values to 0; here we reject outright so an invalid
        // argument cannot be silently treated as "no flush")
        if (flush_at < 0) die("flushAt must be >= 0");
    }

    std::vector<uint8_t> in = read_all_stdin();
    std::vector<uint8_t> out = compress_framed(in, level, mtime, os, flush_at);
    write_all_stdout(out.data(), out.size());
    return 0;
}

// stream mode: drive raw deflate with an explicit (flush, length) call
// sequence — the C-side twin of the Go Deflater's Deflate(p, flush) calls
// (full input slice as avail_in, always-sufficient avail_out).
int cmd_stream(int argc, char** argv) {
    if (argc < 4) die("usage: stream <level> <flush>@<len> [<flush>@<len> ...]");
    int level = parse_int(argv[2]);
    std::vector<uint8_t> in = read_all_stdin();

    z_stream s;
    memset(&s, 0, sizeof(s));
    if (deflateInit2(&s, level, Z_DEFLATED, -15, 8, Z_DEFAULT_STRATEGY) != Z_OK) {
        die("deflateInit2 failed");
    }

    // deflateBound covers the data; each flush op adds at most a few bytes
    // of empty-block/sync overhead
    size_t max = deflateBound(&s, in.size()) + 64 * static_cast<size_t>(argc);
    std::vector<uint8_t> out(max);
    s.next_out = out.data();
    s.avail_out = static_cast<uInt>(max);

    size_t pos = 0;
    for (int i = 3; i < argc; i++) {
        const char* at = strchr(argv[i], '@');
        if (!at) die("stream op must be <flush>@<len>");
        std::string fs(argv[i], static_cast<size_t>(at - argv[i]));
        int flush = parse_int(fs.c_str());
        long len = parse_int(at + 1);
        if (flush < Z_NO_FLUSH || flush > Z_FINISH) die("flush must be 0..4");
        if (len < 0 || pos + static_cast<size_t>(len) > in.size()) {
            die("stream op length exceeds input");
        }
        s.next_in = const_cast<Bytef*>(in.data() + pos);
        s.avail_in = static_cast<uInt>(len);
        pos += static_cast<size_t>(len);

        if (flush == Z_FINISH) {
            int r;
            do {
                r = deflate(&s, Z_FINISH);
            } while (r == Z_OK); // level 0 may need more than one call
            if (r != Z_STREAM_END) die("deflate(Z_FINISH) failed");
        } else {
            // With sufficient avail_out one call consumes all input;
            // Z_BUF_ERROR just means "nothing to do" (e.g. repeated flush
            // with no new input) and is not fatal, as in C usage
            int r = deflate(&s, flush);
            if (r != Z_OK && r != Z_BUF_ERROR) die("deflate failed");
            if (s.avail_in != 0) die("deflate did not consume the op's input");
        }
    }
    if (pos != in.size()) die("stream ops did not consume the whole input");

    size_t out_len = max - s.avail_out;
    deflateEnd(&s);
    write_all_stdout(out.data(), out_len);
    return 0;
}

int cmd_header(int argc, char** argv) {
    if (argc != 8) die("usage: header <level> <mtime> <os> <nameHex> <commentHex> <extraHex>");
    int level = parse_int(argv[2]);
    uint32_t mtime = static_cast<uint32_t>(strtoul(argv[3], nullptr, 10));
    uint8_t os = static_cast<uint8_t>(parse_int(argv[4]));

    std::vector<uint8_t> name, comment, extra;
    bool has_name = strcmp(argv[5], "-") != 0;
    bool has_comment = strcmp(argv[6], "-") != 0;
    bool has_extra = strcmp(argv[7], "-") != 0;
    if (has_name) name = parse_hex(argv[5]);
    if (has_comment) comment = parse_hex(argv[6]);
    if (has_extra) extra = parse_hex(argv[7]);

    std::vector<uint8_t> in = read_all_stdin();
    std::vector<uint8_t> out = compress_header(
        in, level, mtime, os,
        has_name ? &name : nullptr,
        has_comment ? &comment : nullptr,
        has_extra ? &extra : nullptr);
    write_all_stdout(out.data(), out.size());
    return 0;
}

// bench uses the conventional long-running pattern for C programs:
// deflateInit2 once, deflateReset per iteration to reuse the compression
// stream (same usage as nginx/OpenJDK etc., and matching the sync.Pool state
// reuse of both Go sides). Each iteration still performs the full semantics:
// reset + deflate(Z_FINISH) + crc32 + GZIP header/trailer framing.
//
// Note: with deflateInit2/deflateEnd per iteration instead, the glibc
// main-thread arena returns pages after free, so every init page-faults
// again (~40µs of fake overhead) — measuring allocator behavior rather than
// compression performance.
int cmd_bench(int argc, char** argv) {
    if (argc != 4) die("usage: bench <level> <iters>");
    int level = parse_int(argv[2]);
    long iters = parse_int(argv[3]);
    if (iters <= 0) die("iters must be > 0");

    std::vector<uint8_t> in = read_all_stdin();
    const uint32_t mtime = 1751038273u;

    z_stream s;
    memset(&s, 0, sizeof(s));
    if (deflateInit2(&s, level, Z_DEFLATED, -15, 8, Z_DEFAULT_STRATEGY) != Z_OK) {
        die("deflateInit2 failed");
    }
    size_t max = deflateBound(&s, in.size());
    std::vector<uint8_t> out(10 + max + 8);

    size_t out_len = 0;
    auto start = std::chrono::steady_clock::now();
    for (long i = 0; i < iters; i++) {
        if (deflateReset(&s) != Z_OK) die("deflateReset failed");
        s.next_in = const_cast<Bytef*>(in.data());
        s.avail_in = static_cast<uInt>(in.size());
        s.next_out = out.data() + 10;
        s.avail_out = static_cast<uInt>(max);
        int r;
        do {
            r = deflate(&s, Z_FINISH);
        } while (r == Z_OK);
        if (r != Z_STREAM_END) die("deflate(Z_FINISH) failed");
        size_t def_len = max - s.avail_out;

        out[0] = 0x1f; out[1] = 0x8b; out[2] = 0x08; out[3] = 0x00;
        out[4] = static_cast<uint8_t>(mtime);
        out[5] = static_cast<uint8_t>(mtime >> 8);
        out[6] = static_cast<uint8_t>(mtime >> 16);
        out[7] = static_cast<uint8_t>(mtime >> 24);
        out[8] = 0x00; out[9] = 0x03;
        uint32_t crc = static_cast<uint32_t>(
            crc32(0L, in.empty() ? Z_NULL : in.data(), static_cast<uInt>(in.size())));
        uint32_t isize = static_cast<uint32_t>(in.size());
        size_t t = 10 + def_len;
        out[t]     = static_cast<uint8_t>(crc);
        out[t + 1] = static_cast<uint8_t>(crc >> 8);
        out[t + 2] = static_cast<uint8_t>(crc >> 16);
        out[t + 3] = static_cast<uint8_t>(crc >> 24);
        out[t + 4] = static_cast<uint8_t>(isize);
        out[t + 5] = static_cast<uint8_t>(isize >> 8);
        out[t + 6] = static_cast<uint8_t>(isize >> 16);
        out[t + 7] = static_cast<uint8_t>(isize >> 24);
        out_len = t + 8;
    }
    auto end = std::chrono::steady_clock::now();
    deflateEnd(&s);
    double ns = std::chrono::duration<double, std::nano>(end - start).count();

    printf("{\"iters\":%ld,\"ns_per_op\":%.1f,\"in_bytes\":%zu,\"out_bytes\":%zu}\n",
           iters, ns / static_cast<double>(iters), in.size(), out_len);
    return 0;
}

} // namespace

int main(int argc, char** argv) {
    if (argc < 2) die("usage: gzip_ref <compress|stream|header|bench|version> ...");
    if (strcmp(argv[1], "version") == 0) {
        printf("%s\n", zlibVersion());
        return 0;
    }
    if (strcmp(argv[1], "compress") == 0) return cmd_compress(argc, argv);
    if (strcmp(argv[1], "stream") == 0) return cmd_stream(argc, argv);
    if (strcmp(argv[1], "header") == 0) return cmd_header(argc, argv);
    if (strcmp(argv[1], "bench") == 0) return cmd_bench(argc, argv);
    die("unknown subcommand");
}
