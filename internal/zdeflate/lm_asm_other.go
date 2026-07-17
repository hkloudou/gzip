//go:build !amd64 || purego

package zdeflate

// No assembly longestMatch on this target: the pure Go loop is the
// implementation. The stub keeps the call site compilable; it is
// unreachable behind the constant-false gate.
const asmLongestMatch = false

func longestMatchAsm(win *byte, prev *uint16, curMatch, scan, bestLen, chainLength, niceMatch, limit int) (retLen, retStart int) {
	panic("unreachable: asmLongestMatch is false on this target")
}
