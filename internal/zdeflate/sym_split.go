//go:build !gziplowmem

// Default symbol layout: separate distance/literal arrays (the same shape
// as C zlib 1.3.1's optional LIT_MEM mode). This is the speed-first
// choice — the A/B workflow measured the low-memory overlay layout
// (sym_lowmem.go, -tags gziplowmem) slower on both CI architectures
// (EPYC x86-64: Random_1MB +4.6%, level1 +3.8%; arm64: level1 +4.5%,
// geomean +0.78%), so per the repository performance policy speed wins
// the default and the overlay is the opt-in build. Both layouts record
// the identical (dist, lc) stream with identical flush points; CI
// byte-verifies both configurations against the C referees.
package zdeflate

import "encoding/binary"

// symArea holds the split symbol arrays (48KB more state than the
// gziplowmem overlay, in exchange for the measured speed above).
type symArea struct {
	symDist [litBufsize]uint16
	symLc   [litBufsize]byte
}

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
		symLc := s.symLc[:s.symNext]
		symDist := s.symDist[:s.symNext]
		for sx, lcb := range symLc {
			dist := int(symDist[sx])
			lc := int(lcb)
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

// tallyLit corresponds to the _tr_tally_lit macro; returns whether the block must be flushed
func (s *state) tallyLit(c byte) bool {
	s.symDist[s.symNext] = 0
	s.symLc[s.symNext] = c
	s.symNext++
	s.dynLtree[c].fc++
	return s.symNext == symEnd
}

// tallyDist corresponds to the _tr_tally_dist macro
func (s *state) tallyDist(distance, length int) bool {
	s.symDist[s.symNext] = uint16(distance)
	s.symLc[s.symNext] = byte(length)
	s.symNext++
	dist := distance - 1
	s.dynLtree[int(lengthCode[length])+literals+1].fc++
	s.dynDtree[dCode(dist)].fc++
	return s.symNext == symEnd
}
