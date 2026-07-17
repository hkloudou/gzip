//go:build amd64 && !purego

#include "textflag.h"

// func longestMatchAsm(win *byte, prev *uint16, curMatch, scan, bestLen, chainLength, niceMatch, limit int) (retLen, retStart int)
//
// Faithful transcription of longestMatch's chain loop (deflate.go): same
// probes (16-bit head/tail), same 8-byte XOR compare with the final group
// overlapped at 250, same tie-breaks and exits, so the returned
// (length, start) — and every output byte — cannot differ from the Go
// loop. The caller applies the good_match chain reduction, the
// nice_match/lookahead clamp, and the lookahead cap on return. BSFQ is
// TZCNT-compatible on the guaranteed-nonzero operand.
TEXT ·longestMatchAsm(SB), NOSPLIT, $0-80
	MOVQ win+0(FP), SI
	MOVQ prev+8(FP), DI
	MOVQ curMatch+16(FP), AX
	MOVQ scan+24(FP), BX
	MOVQ bestLen+32(FP), DX
	MOVQ chainLength+40(FP), CX
	MOVQ niceMatch+48(FP), R9
	MOVQ limit+56(FP), R8

	MOVQ $-1, R15                // matchStart sentinel (caller keeps s.matchStart)

	MOVWQZX (SI)(BX*1), R10      // scanStart
	LEAQ -1(BX)(DX*1), R13
	MOVWQZX (SI)(R13*1), R11     // scanEnd

loop:
	// tail probe: win[match+bestLen-1..] == scanEnd ?
	LEAQ -1(AX)(DX*1), R13
	MOVWQZX (SI)(R13*1), R14
	CMPQ R14, R11
	JNE  next

	// head probe: win[match..] == scanStart ?
	MOVWQZX (SI)(AX*1), R14
	CMPQ R14, R10
	JNE  next

	MOVQ $3, R13                 // j
cmp8:
	LEAQ (BX)(R13*1), R14
	MOVQ (SI)(R14*1), R14        // win[scan+j]
	LEAQ (AX)(R13*1), R12
	XORQ (SI)(R12*1), R14        // ^ win[match+j]
	JNE  found_diff
	CMPQ R13, $250               // maxMatch-8
	JE   full_match
	ADDQ $8, R13
	CMPQ R13, $250
	JLE  cmp8
	MOVQ $250, R13
	JMP  cmp8

found_diff:
	BSFQ R14, R14
	SHRQ $3, R14
	ADDQ R13, R14                // matchLen = j + tz/8
	JMP  have_len

full_match:
	MOVQ $258, R14               // maxMatch

have_len:
	CMPQ R14, DX
	JLE  next
	MOVQ AX, R15                 // matchStart = curMatch
	MOVQ R14, DX                 // bestLen = matchLen
	CMPQ R14, R9
	JGE  done                    // >= niceMatch
	LEAQ -1(BX)(DX*1), R13
	MOVWQZX (SI)(R13*1), R11     // refresh scanEnd

next:
	MOVQ AX, R12                 // next = prev[cur & wMask]
	ANDQ $0x7fff, R12
	MOVWQZX (DI)(R12*2), AX
	CMPQ AX, R8
	JLE  done                    // cur <= limit
	DECQ CX
	JNZ  loop

done:
	MOVQ DX, retLen+64(FP)
	MOVQ R15, retStart+72(FP)
	RET
