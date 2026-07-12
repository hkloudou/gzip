// Package czlib is a test-only reference implementation backed by the real
// C zlib (internal, not exported).
//
// Production code (the root package's gzip.Writer/Reader) is 100% pure Go
// and does not depend on this package; this package only provides "real C
// zlib output" as a byte-for-byte comparison baseline for tests/CI:
//   - With CGO_ENABLED=1 it compiles the embedded official zlib 1.3.1
//     sources (the zlib/ directory);
//   - With CGO_ENABLED=0 all entry points return ErrRequiresCGO and
//     callers skip the C comparison leg.
package czlib

import (
	"errors"
	"hash/crc32"
)

var (
	// ErrInvalidLevel means the compression level is outside the range
	// zlib allows (-1, 0-9).
	ErrInvalidLevel = errors.New("czlib: invalid compression level")
	// ErrRequiresCGO means this entry point is only available in CGO builds.
	ErrRequiresCGO = errors.New("czlib: requires CGO_ENABLED=1")
	// ErrInputTooLarge means the input exceeds the single-call limit
	// (zlib's avail_in is 32-bit; anything over 4GiB-1 would be silently
	// truncated, so we reject it explicitly).
	ErrInputTooLarge = errors.New("czlib: input exceeds 4GiB single-shot limit")
)

func normalizeLevel(level int) (int, error) {
	if level == -1 {
		return 6, nil // Z_DEFAULT_COMPRESSION, same as zlib
	}
	if level < 0 || level > 9 {
		return 0, ErrInvalidLevel
	}
	return level, nil
}

// gzipFrameOpts assembles a GZIP frame from raw deflate data, byte-identical
// to frame_gzip in gzip.c:
// [1f 8b 08 00 <ts LE> 00 <os>][raw][crc32 LE][isize LE]
func gzipFrameOpts(raw, input []byte, ts uint32, osByte byte) []byte {
	out := make([]byte, 10+len(raw)+8)
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
	copy(out[10:], raw)
	crc := crc32.ChecksumIEEE(input)
	t := 10 + len(raw)
	out[t] = byte(crc)
	out[t+1] = byte(crc >> 8)
	out[t+2] = byte(crc >> 16)
	out[t+3] = byte(crc >> 24)
	n := uint32(len(input))
	out[t+4] = byte(n)
	out[t+5] = byte(n >> 8)
	out[t+6] = byte(n >> 16)
	out[t+7] = byte(n >> 24)
	return out
}
