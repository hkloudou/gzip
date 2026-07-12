//go:build darwin && cgo
// +build darwin,cgo

package czlib

// Empirical probes for Apple platforms (test-only):
//  1. Apple Compression framework (COMPRESSION_ZLIB) — Apple's own encoder
//  2. System /usr/lib/libz.1.dylib — called via dlopen, fully isolated
//     from the embedded zlib
// Used on macOS CI to verify byte-for-byte consistency with standard zlib.

/*
#cgo LDFLAGS: -lcompression
#include <stdlib.h>
#include <string.h>
#include <dlfcn.h>
#include <compression.h>
#include "zlib.h"

// Apple Compression framework: COMPRESSION_ZLIB produces raw deflate
static uint8_t* apple_cf_zlib(const uint8_t* in, size_t n, size_t* out_len) {
	size_t cap = n + (n >> 1) + 4096;
	uint8_t* dst = malloc(cap);
	if (!dst) return NULL;
	size_t r = compression_encode_buffer(dst, cap, in, n, NULL, COMPRESSION_ZLIB);
	if (r == 0) { free(dst); return NULL; }
	*out_len = r;
	return dst;
}

typedef int (*p_deflateInit2_)(z_streamp, int, int, int, int, int, const char*, int);
typedef int (*p_deflate)(z_streamp, int);
typedef int (*p_deflateEnd)(z_streamp);
typedef const char* (*p_zlibVersion)(void);

static void* sys_libz_handle(void) {
	return dlopen("/usr/lib/libz.1.dylib", RTLD_NOW);
}

static const char* sys_libz_version(void) {
	void* h = sys_libz_handle();
	if (!h) return "";
	p_zlibVersion v = (p_zlibVersion)dlsym(h, "zlibVersion");
	return v ? v() : "";
}

// Raw deflate via the system libz (windowBits=-15, memLevel=8, default strategy)
static uint8_t* sys_libz_deflate_raw(const uint8_t* in, size_t n, int level, size_t* out_len) {
	void* h = sys_libz_handle();
	if (!h) return NULL;
	p_deflateInit2_ init2 = (p_deflateInit2_)dlsym(h, "deflateInit2_");
	p_deflate def = (p_deflate)dlsym(h, "deflate");
	p_deflateEnd end = (p_deflateEnd)dlsym(h, "deflateEnd");
	p_zlibVersion ver = (p_zlibVersion)dlsym(h, "zlibVersion");
	if (!init2 || !def || !end || !ver) return NULL;

	z_stream s;
	memset(&s, 0, sizeof(s));
	if (init2(&s, level, Z_DEFLATED, -15, 8, Z_DEFAULT_STRATEGY,
	          ver(), (int)sizeof(z_stream)) != Z_OK) {
		return NULL;
	}
	size_t cap = n + (n >> 8) + 1024;
	uint8_t* dst = malloc(cap);
	if (!dst) { end(&s); return NULL; }
	s.next_in = (Bytef*)in;
	s.avail_in = (uInt)n;
	s.next_out = dst;
	s.avail_out = (uInt)cap;
	int r;
	do { r = def(&s, Z_FINISH); } while (r == Z_OK);
	if (r != Z_STREAM_END) { free(dst); end(&s); return NULL; }
	*out_len = cap - s.avail_out;
	end(&s);
	return dst;
}
*/
import "C"
import (
	"errors"
	"unsafe"
)

// appleCompressionZlib calls the Apple Compression framework (COMPRESSION_ZLIB).
func appleCompressionZlib(input []byte) ([]byte, error) {
	if len(input) == 0 {
		return nil, errors.New("empty input")
	}
	var outLen C.size_t
	p := C.apple_cf_zlib(
		(*C.uint8_t)(unsafe.Pointer(&input[0])),
		C.size_t(len(input)),
		&outLen,
	)
	if p == nil {
		return nil, errors.New("compression_encode_buffer failed")
	}
	defer C.free(unsafe.Pointer(p))
	return C.GoBytes(unsafe.Pointer(p), C.int(outLen)), nil
}

// appleSystemLibzVersion returns the system libz's version string.
func appleSystemLibzVersion() string {
	return C.GoString(C.sys_libz_version())
}

// appleSystemLibzDeflateRaw does raw deflate using the system /usr/lib/libz.1.dylib.
func appleSystemLibzDeflateRaw(input []byte, level int) ([]byte, error) {
	if len(input) == 0 {
		return nil, errors.New("empty input")
	}
	var outLen C.size_t
	p := C.sys_libz_deflate_raw(
		(*C.uint8_t)(unsafe.Pointer(&input[0])),
		C.size_t(len(input)),
		C.int(level),
		&outLen,
	)
	if p == nil {
		return nil, errors.New("system libz deflate failed")
	}
	defer C.free(unsafe.Pointer(p))
	return C.GoBytes(unsafe.Pointer(p), C.int(outLen)), nil
}
