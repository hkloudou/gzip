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

    /* empty input and failure paths must not leak */
    r = gzip_compress_sync(data, 0, 0u, 6, 3, 0);
    assert(r);
    free(r);
    assert(deflate_stream_new(99) == NULL); /* invalid level: failed init must free the handle */

    free(data);
    printf("leak_check: OK\n");
    return 0;
}
