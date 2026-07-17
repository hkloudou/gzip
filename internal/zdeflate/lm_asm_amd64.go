//go:build amd64 && !purego

package zdeflate

// asmLongestMatch gates the assembly longestMatch (lm_amd64.s). OFF until
// the A/B verdict on real CI hardware shows it beating the Go loop on this
// architecture — the owner's rule: assembly that does not win falls back
// to Go. Byte-parity of the asm path is verified by forcing this on
// (1330 x 3 referees x both flavors + fuzz) before any ship decision.
const asmLongestMatch = false

// longestMatchAsm is implemented in lm_amd64.s. It returns the best match
// length and the winning start position (-1 if no candidate beat bestLen,
// in which case the caller leaves s.matchStart untouched, exactly like the
// Go loop).
//
//go:noescape
func longestMatchAsm(win *byte, prev *uint16, curMatch, scan, bestLen, chainLength, niceMatch, limit int) (retLen, retStart int)
