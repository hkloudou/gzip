// One-shot compression API and gzip framing (output is byte-identical to
// C zlib with the same parameters).
package zdeflate

import (
	"hash/crc32"
	"sync"
)

// Bound returns an upper bound on the output size for compressing n input
// bytes. Matches deflateBound's tight bound under the default parameters
// (windowBits=15, memLevel=8, raw).
func Bound(n int) int {
	return n + (n >> 12) + (n >> 14) + (n >> 25) + 13 - 6
}

// Pool of intermediate output buffers. Compression first writes into a
// pooled scratch buffer, then copies out an exactly-sized result, so we
// don't return slices whose cap far exceeds len and pin memory long-term
// (at high compression ratios bound ≈ input size). Scratch buffers larger
// than maxPooledBuf are not pooled, so occasional huge inputs don't bloat
// the pool.
const maxPooledBuf = 4 << 20 // 4MB

var scratchPool = sync.Pool{}

func getScratch(n int) []byte {
	if v := scratchPool.Get(); v != nil {
		b := v.([]byte)
		if cap(b) >= n {
			return b[:n]
		}
	}
	return make([]byte, n)
}

func putScratch(b []byte) {
	if cap(b) <= maxPooledBuf {
		scratchPool.Put(b[:0]) //nolint:staticcheck // slice header escape is acceptable
	}
}

// compressInto compresses input as raw deflate into out and returns the
// number of bytes written. Equivalent to the C sequence:
//
//	deflateInit2(&s, level, Z_DEFLATED, -15, 8, Z_DEFAULT_STRATEGY);
//	deflate(&s, Z_FINISH);
//
// out must be at least Bound(len(input)) bytes, otherwise (0, false) is
// returned.
func compressInto(out, input []byte, level int) (int, bool) {
	s := statePool.Get().(*state)
	s.reset(level)
	s.in = input
	s.inPos = 0
	s.out = out
	s.outPos = 0

	bs := s.deflateOnce(zFinish)

	n := s.outPos
	ok := (bs == finishDone || bs == finishStarted) && s.pending == 0
	// Clear references so the pool doesn't pin the caller's buffers
	s.in = nil
	s.out = nil
	statePool.Put(s)
	return n, ok
}

// CompressLevel performs a one-shot raw deflate at the given level (0-9,
// -1 means default 6); output is byte-identical to C zlib with the same
// parameters. The returned slice is a freshly allocated, exactly-sized copy.
func CompressLevel(input []byte, level int) []byte {
	if level == -1 {
		level = 6
	}
	if level < 0 || level > 9 {
		panic("zdeflate: invalid compression level")
	}
	scratch := getScratch(Bound(len(input)))
	n, ok := compressInto(scratch, input, level)
	if !ok {
		// Theoretically unreachable: Bound is zlib's proven upper bound.
		// Kept as a safety net.
		panic("zdeflate: output bound exceeded")
	}
	res := make([]byte, n)
	copy(res, scratch[:n])
	putScratch(scratch)
	return res
}

// GzipCompressLevel produces GZIP data at the given level (0-9, -1 means
// default 6, matching zlib):
//
//	[10-byte header: 1f 8b 08 00 <ts little-endian 4 bytes> 00 <os>]
//	[raw deflate]
//	[8-byte trailer: crc32 little-endian, isize little-endian]
//
// The returned slice is a freshly allocated, exactly-sized copy.
func GzipCompressLevel(input []byte, ts uint32, level int, osByte byte) []byte {
	if level == -1 {
		level = 6
	}
	if level < 0 || level > 9 {
		panic("zdeflate: invalid compression level")
	}

	scratch := getScratch(Bound(len(input)))
	n, ok := compressInto(scratch, input, level)
	if !ok {
		panic("zdeflate: output bound exceeded")
	}

	out := make([]byte, 10+n+8)

	// GZIP Header (10 bytes)
	out[0] = 0x1f
	out[1] = 0x8b
	out[2] = 0x08
	out[3] = 0x00
	out[4] = byte(ts)
	out[5] = byte(ts >> 8)
	out[6] = byte(ts >> 16)
	out[7] = byte(ts >> 24)
	out[8] = 0x00
	out[9] = osByte

	copy(out[10:], scratch[:n])
	putScratch(scratch)

	// GZIP Trailer (8 bytes)
	crc := crc32.ChecksumIEEE(input)
	t := 10 + n
	out[t] = byte(crc)
	out[t+1] = byte(crc >> 8)
	out[t+2] = byte(crc >> 16)
	out[t+3] = byte(crc >> 24)
	inLen := uint32(len(input))
	out[t+4] = byte(inLen)
	out[t+5] = byte(inLen >> 8)
	out[t+6] = byte(inLen >> 16)
	out[t+7] = byte(inLen >> 24)

	return out
}
