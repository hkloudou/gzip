//go:build !amd64 && !arm64

package zdeflate

import "encoding/binary"

// Portable window loads for longestMatch (see load_fast.go for the
// specialized amd64/arm64 versions and the equivalence proof).
//
// load16m loads window bytes i and i+1 as a little-endian 16-bit value with
// masked indexing. Every probe position in longestMatch satisfies
// i+1 < windowSize (match < strstart <= windowSize-minLookahead and
// bestLen <= maxMatch keep the probes inside the window), so the masks are
// identity: the same two bytes are loaded, the masks only let the compiler
// drop the bounds checks in the chain-walking loop.
func load16m(win *[windowSize]byte, i int) uint16 {
	return uint16(win[i&(windowSize-1)]) | uint16(win[(i+1)&(windowSize-1)])<<8
}

// load64w loads 8 window bytes at i, little-endian.
func load64w(win *[windowSize]byte, i int) uint64 {
	return binary.LittleEndian.Uint64(win[i&(windowSize-1):])
}
