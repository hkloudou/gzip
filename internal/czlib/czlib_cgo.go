//go:build cgo
// +build cgo

package czlib

/*
#cgo CFLAGS: -I${SRCDIR}/zlib
#include <stdint.h>
#include <stdlib.h>

// Amalgamated compilation unit for zlib's compression side (cgo does not
// automatically compile the zlib/ subdirectory, so it is pulled in here)
#include "zlib/zlib_amalgam.c"

// gzip.c lives in the package directory and is compiled by cgo
// automatically; only its entry-point prototypes are declared here
extern void* deflate_stream_new(int level);
extern void deflate_stream_free(void* h);
extern uint8_t* deflate_stream_oneshot(void* h, const uint8_t* input, size_t in_len);
extern uint8_t* gzip_compress_sync(const uint8_t* input, size_t in_len,
                                   uint32_t ts, int level, uint8_t os, size_t split);
extern uint8_t* gzip_compress_header(const uint8_t* input, size_t in_len,
                                     int level, uint32_t ts, uint8_t os,
                                     const uint8_t* extra, uint32_t extra_len,
                                     const char* name, const char* comment);
extern uint8_t* deflate_raw_stream(const uint8_t* input, size_t in_len, int level,
                                   const uint32_t* chunk_lens, const int32_t* flushes,
                                   int32_t n_ops);
*/
import "C"
import (
	"encoding/binary"
	"errors"
	"runtime"
	"sync"
	"unsafe"
)

// HasCGO reports whether CGO is enabled.
func HasCGO() bool {
	return true
}

// CompressOpts compresses with the real C zlib; ts, level, and the OS byte
// are all configurable.
func CompressOpts(input []byte, ts uint32, level int, osByte byte) ([]byte, error) {
	lv, err := normalizeLevel(level)
	if err != nil {
		return nil, err
	}
	if len(input) == 0 {
		return nil, nil
	}
	raw, err := cgoDeflateRaw(input, lv)
	if err != nil {
		return nil, err
	}
	return gzipFrameOpts(raw, input, ts, osByte), nil
}

// CompressWithSyncFlush performs one Z_SYNC_FLUSH at splitAt before
// finishing. Demo/verification only: a sync flush changes the compressed
// bytes (the decompressed result is unchanged).
func CompressWithSyncFlush(input []byte, ts uint32, level int, osByte byte, splitAt int) ([]byte, error) {
	lv, err := normalizeLevel(level)
	if err != nil {
		return nil, err
	}
	if len(input) == 0 {
		return nil, nil
	}
	if err := checkCgoInputSize(input); err != nil {
		return nil, err
	}
	if splitAt < 0 {
		splitAt = 0
	}
	result := C.gzip_compress_sync(
		(*C.uint8_t)(unsafe.Pointer(&input[0])),
		C.size_t(len(input)),
		C.uint32_t(ts),
		C.int(lv),
		C.uint8_t(osByte),
		C.size_t(splitAt),
	)
	return copyCResult(result)
}

// latin1Bytes encodes a Go string as Latin-1 (ISO 8859-1) bytes, with the
// same semantics as gzip.Writer's header string output; it errors on NUL
// or non-Latin-1 characters.
func latin1Bytes(s string) ([]byte, error) {
	b := make([]byte, 0, len(s))
	for _, v := range s {
		if v == 0 || v > 0xff {
			return nil, errors.New("czlib: non-Latin-1 header string")
		}
		b = append(b, byte(v))
	}
	return b, nil
}

var emptyExtraByte [1]byte

// CompressWithGzHeader has the real C zlib produce a complete GZIP stream
// (windowBits=15+16 plus deflateSetHeader), with optional extra/name/comment
// header fields. Test-only: compared byte-for-byte against gzip.Writer's
// header output.
// Note: on this path C zlib computes XFL from the level (9→2, <2→4,
// otherwise→0), while this library's Writer always writes XFL=0, so byte 8
// must be adjusted when comparing.
func CompressWithGzHeader(input []byte, level int, ts uint32, osByte byte,
	extra []byte, name, comment string) ([]byte, error) {
	lv, err := normalizeLevel(level)
	if err != nil {
		return nil, err
	}
	if err := checkCgoInputSize(input); err != nil {
		return nil, err
	}

	var inPtr *C.uint8_t
	if len(input) > 0 {
		inPtr = (*C.uint8_t)(unsafe.Pointer(&input[0]))
	}

	// extra: nil = don't write FEXTRA; non-nil empty slice = write FEXTRA
	// with xlen=0 (matching the standard library's `Extra != nil` semantics)
	var extraPtr *C.uint8_t
	if extra != nil {
		if len(extra) > 0 {
			extraPtr = (*C.uint8_t)(unsafe.Pointer(&extra[0]))
		} else {
			extraPtr = (*C.uint8_t)(unsafe.Pointer(&emptyExtraByte[0]))
		}
	}

	var namePtr, commentPtr *C.char
	if name != "" {
		nb, err := latin1Bytes(name)
		if err != nil {
			return nil, err
		}
		namePtr = (*C.char)(C.CBytes(append(nb, 0)))
		defer C.free(unsafe.Pointer(namePtr))
	}
	if comment != "" {
		cb, err := latin1Bytes(comment)
		if err != nil {
			return nil, err
		}
		commentPtr = (*C.char)(C.CBytes(append(cb, 0)))
		defer C.free(unsafe.Pointer(commentPtr))
	}

	result := C.gzip_compress_header(
		inPtr, C.size_t(len(input)),
		C.int(lv), C.uint32_t(ts), C.uint8_t(osByte),
		extraPtr, C.uint32_t(len(extra)),
		namePtr, commentPtr,
	)
	return copyCResult(result)
}

// cgoDeflateRawStream performs streaming raw deflate with C zlib following
// a (chunk, flush) call sequence; used only to compare against the pure Go
// streaming implementation.
func cgoDeflateRawStream(input []byte, level int, chunks []uint32, flushes []int32) ([]byte, error) {
	if len(chunks) == 0 || len(chunks) != len(flushes) {
		return nil, errors.New("czlib: invalid stream ops")
	}
	if err := checkCgoInputSize(input); err != nil {
		return nil, err
	}
	var inPtr *C.uint8_t
	if len(input) > 0 {
		inPtr = (*C.uint8_t)(unsafe.Pointer(&input[0]))
	}
	result := C.deflate_raw_stream(
		inPtr,
		C.size_t(len(input)),
		C.int(level),
		(*C.uint32_t)(unsafe.Pointer(&chunks[0])),
		(*C.int32_t)(unsafe.Pointer(&flushes[0])),
		C.int32_t(len(chunks)),
	)
	return copyCResult(result)
}

// copyCResult copies a C return value of the form [len:4][data] and frees it.
// C.GoBytes is not used because its length parameter is C.int (int32): an
// outLen ≥ 2GiB would go negative and trigger a runtime panic; instead we
// copy at full width via unsafe.Slice.
func copyCResult(result *C.uint8_t) ([]byte, error) {
	if result == nil {
		return nil, errors.New("czlib: compression failed")
	}
	defer C.free(unsafe.Pointer(result))
	header := C.GoBytes(unsafe.Pointer(result), 4)
	outLen := binary.LittleEndian.Uint32(header)
	if uint64(outLen) > uint64(maxInt) {
		return nil, ErrInputTooLarge // cannot fit on 32-bit platforms
	}
	src := unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(result), 4)), int(outLen))
	out := make([]byte, outLen)
	copy(out, src)
	return out, nil
}

const maxInt = int(^uint(0) >> 1)

/* ------------------------------------------------------------------ */
/* C-side compression stream pool: mirrors the pure Go sync.Pool,      */
/* avoiding per-call deflateInit2/deflateEnd (glibc heap trim makes    */
/* them cost a spurious ~40µs)                                         */
/* ------------------------------------------------------------------ */

// cgoStream wraps a long-lived C z_stream. When evicted from the sync.Pool
// its C memory is freed by a GC finalizer (objects still in the pool are
// reachable and won't be reclaimed by mistake).
type cgoStream struct {
	h unsafe.Pointer
}

// One pool per level (deflateReset does not change the level)
var cgoStreamPools [10]sync.Pool

// getCgoStream fetches (or creates) a reusable compression stream for the
// given level. level must already be normalized.
func getCgoStream(level int) (*cgoStream, error) {
	if v := cgoStreamPools[level].Get(); v != nil {
		return v.(*cgoStream), nil
	}
	h := C.deflate_stream_new(C.int(level))
	if h == nil {
		return nil, errors.New("czlib: deflateInit2 failed")
	}
	s := &cgoStream{h: h}
	runtime.SetFinalizer(s, func(s *cgoStream) {
		C.deflate_stream_free(s.h)
	})
	return s, nil
}

// cgoDeflateRaw calls C zlib to do raw deflate at any level (reusing pooled
// streams; deflateReset guarantees output byte-identical to a fresh stream).
// level must already be normalized (0-9).
func cgoDeflateRaw(input []byte, level int) ([]byte, error) {
	if err := checkCgoInputSize(input); err != nil {
		return nil, err
	}
	s, err := getCgoStream(level)
	if err != nil {
		return nil, err
	}
	var ptr *C.uint8_t
	if len(input) > 0 {
		ptr = (*C.uint8_t)(unsafe.Pointer(&input[0]))
	}
	result := C.deflate_stream_oneshot(s.h, ptr, C.size_t(len(input)))
	runtime.KeepAlive(s) // keep s alive during the C call so GC/finalizer can't reclaim it
	cgoStreamPools[level].Put(s)
	if result == nil {
		return nil, errors.New("czlib: deflate failed")
	}
	return copyCResult(result)
}

// checkCgoInputSize rejects inputs beyond what zlib's 32-bit avail_in can
// represent: overflow would silently truncate and produce corrupt output.
// The pure Go path has no such limit.
func checkCgoInputSize(input []byte) error {
	if uint64(len(input)) > 0xFFFFFFFF {
		return ErrInputTooLarge
	}
	return nil
}
