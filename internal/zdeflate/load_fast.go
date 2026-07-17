//go:build amd64 || arm64

package zdeflate

import "unsafe"

// Architecture-specialized window loads for longestMatch's hot loops.
// amd64 and arm64 are little-endian and support unaligned loads, so a
// single raw load replaces the byte-assembly of the portable versions in
// load_portable.go. Byte-invariance proof:
//
//   - the loaded VALUES are identical to the portable versions (same bytes,
//     little-endian order, same masked in-window addresses — the masks are
//     identity for every caller, see the callers' invariants — so equality
//     comparisons and XOR results are bit-identical);
//   - load16m results are only ever compared for equality, and load64w
//     results are only XORed with each other and fed to TrailingZeros64,
//     which on these little-endian loads yields exactly the first
//     differing byte in memory order — the same byte the portable
//     little-endian versions find.
//
// The pure-Go fallback keeps every other GOARCH (including big-endian and
// strict-alignment targets) fully correct; both paths are covered by the
// byte-parity matrices on the architectures CI runs (x86-64, arm64,
// darwin/arm64), and the portable path additionally by the cross-build job.
func load16m(win *[windowSize]byte, i int) uint16 {
	return *(*uint16)(unsafe.Add(unsafe.Pointer(win), uintptr(i&(windowSize-1))))
}

func load64w(win *[windowSize]byte, i int) uint64 {
	return *(*uint64)(unsafe.Add(unsafe.Pointer(win), uintptr(i&(windowSize-1))))
}
