//go:build gziplowmem

// Low-memory symbol layout (build with -tags gziplowmem): C zlib's exact
// default (non-LIT_MEM) sym_buf/pending_buf overlay — the symbol stream
// lives inside pendingBuf, 3 bytes per symbol at litBufsize + 3*symNext
// (dist-low, dist-high, lc), saving 48KB of compressor state versus the
// default split layout. Output bytes are identical to the default build
// (same symbols, same flush points); CI byte-verifies both configurations.
//
// Safety is zlib's own overlay invariant (deflate.c, "We overlay
// pending_buf and sym_buf"): the longest fixed-code length/distance pair
// emits 31 bits while its stored form is 24 bits, and sym_buf starts
// 8*litBufsize bits into pending_buf, so the compressed bits trail the
// unread symbols by at least 139 bits at all times; dynamic blocks are
// only ever chosen when smaller than the fixed encoding, so the same
// bound covers them. Our 64-bit accumulator only widens that margin: bits
// park in the accumulator and reach pendingBuf in whole 4/6-byte groups,
// so the write position is never past ceil(emitted_bits/8), at most 7
// bits beyond C's bit-exact write position — well inside the slack.
//
// Measured cost of this layout (the reason it is not the default; A/B
// workflow, 10 interleaved rounds, p=0.000 unless noted): EPYC x86-64
// Random_1MB +4.6%, level1 +3.8%, JSON_64KB -3.5%, WriterStream -4.3%,
// geomean +0.03%; arm64 level1 +4.5%, geomean +0.78%.
package zdeflate

import "encoding/binary"

// symArea is empty: symbols overlay pendingBuf.
type symArea struct{}

// compressBlock sends the block data using the given trees.
//
// It emits exactly the bit sequence of the C loop (same codes, same extra
// bits, same order), but accumulates into local copies of biBuf/biValid so
// the compiler keeps them in registers across the symbol loop. Whole bytes
// are moved to pendingBuf in 32-bit groups whenever 32 or more bits are
// buffered, checked once after each code+extra pair: a pair adds at most
// maxBits+13 == 28 bits, so biValid never exceeds 31+28 == 59 < 64 and no
// bit is ever lost. Only the flush points differ from sendBits — as with
// the 64-bit accumulator itself, pendingBuf receives the same bytes in the
// same order (whole bytes drained bottom-first), so the output is unchanged.
func (s *state) compressBlock(ltree, dtree []ctData) {
	buf, valid, pending := s.biBuf, s.biValid, s.pending
	if s.symNext != 0 {
		symBuf := s.pendingBuf[litBufsize : litBufsize+3*s.symNext]
		for sx := 0; sx < len(symBuf); sx += 3 {
			dist := int(symBuf[sx]) | int(symBuf[sx+1])<<8
			lc := int(symBuf[sx+2])
			if dist == 0 {
				e := ltree[lc] // literal
				buf |= uint64(e.fc) << uint(valid)
				valid += int(e.dl)
			} else {
				// lc is match length - MIN_MATCH
				code := int(lengthCode[lc])
				e := ltree[code+literals+1]
				buf |= uint64(e.fc) << uint(valid)
				valid += int(e.dl)
				if extra := extraLbits[code]; extra != 0 {
					buf |= uint64(lc-baseLength[code]) << uint(valid)
					valid += extra
				}
				if valid >= 32 {
					binary.LittleEndian.PutUint32(s.pendingBuf[pending:], uint32(buf))
					pending += 4
					buf >>= 32
					valid -= 32
				}
				dist-- // dist becomes match distance - 1
				code = dCode(dist)
				e = dtree[code]
				buf |= uint64(e.fc) << uint(valid)
				valid += int(e.dl)
				if extra := extraDbits[code]; extra != 0 {
					buf |= uint64(dist-baseDist[code]) << uint(valid)
					valid += extra
				}
			}
			if valid >= 32 {
				binary.LittleEndian.PutUint32(s.pendingBuf[pending:], uint32(buf))
				pending += 4
				buf >>= 32
				valid -= 32
			}
		}
	}
	e := ltree[endBlock]
	buf |= uint64(e.fc) << uint(valid)
	valid += int(e.dl)
	s.biBuf, s.biValid, s.pending = buf, valid, pending
}

// tallyLit corresponds to the _tr_tally_lit macro; returns whether the block
// must be flushed. Symbols live inside pendingBuf exactly as in C (see the
// overlay comment on the state struct): base = litBufsize + 3*symNext.
func (s *state) tallyLit(c byte) bool {
	base := litBufsize + 3*s.symNext
	s.pendingBuf[base] = 0
	s.pendingBuf[base+1] = 0
	s.pendingBuf[base+2] = c
	s.symNext++
	s.dynLtree[c].fc++
	return s.symNext == symEnd
}

// tallyDist corresponds to the _tr_tally_dist macro (byte order as in C:
// dist-low, dist-high, lc)
func (s *state) tallyDist(distance, length int) bool {
	base := litBufsize + 3*s.symNext
	s.pendingBuf[base] = byte(distance)
	s.pendingBuf[base+1] = byte(distance >> 8)
	s.pendingBuf[base+2] = byte(length)
	s.symNext++
	dist := distance - 1
	s.dynLtree[int(lengthCode[length])+literals+1].fc++
	s.dynDtree[dCode(dist)].fc++
	return s.symNext == symEnd
}
