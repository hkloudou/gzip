// This file corresponds to zlib's deflate.c (fixed windowBits=15 / memLevel=8 /
// Z_DEFAULT_STRATEGY, i.e. the default zlib parameters in the iOS/Java ecosystems).
package zdeflate

import (
	"encoding/binary"
	"math/bits"
	"sync"
	"unsafe"
)

const (
	minMatch = 3
	maxMatch = 258

	wSize      = 1 << 15 // 32K LZ77 window (windowBits=15)
	wMask      = wSize - 1
	windowSize = 2 * wSize

	hashBits  = 8 + 7 // memLevel=8
	hashSize  = 1 << hashBits
	hashMask  = hashSize - 1
	hashShift = (hashBits + minMatch - 1) / minMatch // 5

	litBufsize     = 1 << (8 + 6) // 16384 (memLevel=8)
	symEnd         = litBufsize - 1
	pendingBufSize = litBufsize * 4

	minLookahead = maxMatch + minMatch + 1 // 262
	maxDistance  = wSize - minLookahead    // MAX_DIST
	winInit      = maxMatch

	nilPos    = 0    // tail of the hash chains
	tooFar    = 4096 // length-3 matches with distance > TOO_FAR are discarded
	maxStored = 65535
)

// flush values, identical to zlib
const (
	zNoFlush      = 0
	zPartialFlush = 1
	zSyncFlush    = 2
	zFullFlush    = 3
	zFinish       = 4
)

// stream status
const (
	busyState   = 113
	finishState = 666
)

type blockState int

const (
	needMore blockState = iota // block not completed, need more input or more output space
	blockDone
	finishStarted
	finishDone
)

// Parameters per compression level, identical to zlib's configuration_table
type config struct {
	goodLength int
	maxLazy    int
	niceLength int
	maxChain   int
	fn         func(*state, int) blockState
}

var configurationTable = [10]config{
	/* 0 */ {0, 0, 0, 0, deflateStored},
	/* 1 */ {4, 4, 8, 4, deflateFast},
	/* 2 */ {4, 5, 16, 8, deflateFast},
	/* 3 */ {4, 6, 32, 32, deflateFast},
	/* 4 */ {4, 4, 16, 16, deflateSlow},
	/* 5 */ {8, 16, 32, 32, deflateSlow},
	/* 6 */ {8, 16, 128, 128, deflateSlow},
	/* 7 */ {8, 32, 128, 256, deflateSlow},
	/* 8 */ {32, 128, 258, 1024, deflateSlow},
	/* 9 */ {32, 258, 258, 4096, deflateSlow},
}

// state corresponds to deflate_state (one-shot compression, raw deflate, wrap=0)
type state struct {
	// stream (one-shot: all input available, output buffer large enough)
	in     []byte
	inPos  int
	out    []byte
	outPos int

	pendingBuf [pendingBufSize]byte
	pendingOut int // next byte of pending_buf to output
	pending    int // number of bytes in pending_buf

	window [windowSize]byte
	prev   [wSize]uint16    // previous position with the same hash value
	head   [hashSize]uint16 // heads of the hash chains

	insH       uint32
	blockStart int // may be negative (after the window slides down)

	matchLength    int // length of best match
	prevMatch      int // match position of the previous step
	matchAvailable bool
	strstart       int // start of string to insert
	matchStart     int
	lookahead      int

	prevLength     int
	maxChainLength int
	maxLazyMatch   int
	level          int
	goodMatch      int
	niceMatch      int

	insert    int
	highWater int

	// fields used by trees.c
	dynLtree [heapSize]ctData
	dynDtree [2*dCodes + 1]ctData
	blTree   [2*blCodes + 1]ctData

	lDesc  treeDesc
	dDesc  treeDesc
	blDesc treeDesc

	blCount [maxBits + 1]uint16
	heap    [2*lCodes + 1]int
	heapLen int
	heapMax int
	depth   [2*lCodes + 1]uint8

	// Symbol buffer: in C, sym_buf shares the same memory as pending_buf
	// (3 bytes/symbol); here it is split into two equivalent arrays with the same capacity semantics
	symDist [litBufsize]uint16
	symLc   [litBufsize]byte
	symNext int // number of buffered symbols; the block is flushed at symEnd
	matches int

	optLen    uint64
	staticLen uint64

	biBuf   uint64 // bit output accumulator, least significant bits first
	biValid int    // number of valid bits in biBuf

	status    int // busyState / finishState
	lastFlush int // flush param of the previous deflate call (C: last_flush)
}

var statePool = sync.Pool{New: func() interface{} { return new(state) }}

func (s *state) availIn() int  { return len(s.in) - s.inPos }
func (s *state) availOut() int { return len(s.out) - s.outPos }

// reset corresponds to deflateResetKeep + lm_init
func (s *state) reset(level int) {
	s.pending = 0
	s.pendingOut = 0
	s.trInit()

	// lm_init
	for i := range s.head {
		s.head[i] = nilPos
	}
	cfg := &configurationTable[level]
	s.maxLazyMatch = cfg.maxLazy
	s.goodMatch = cfg.goodLength
	s.niceMatch = cfg.niceLength
	s.maxChainLength = cfg.maxChain

	s.strstart = 0
	s.blockStart = 0
	s.lookahead = 0
	s.insert = 0
	s.matchLength = minMatch - 1
	s.prevLength = minMatch - 1
	s.matchAvailable = false
	s.insH = 0
	s.highWater = 0
	s.level = level
	s.status = busyState // wrap=0: INIT_STATE goes straight to BUSY_STATE
	s.lastFlush = -2
}

func rank(f int) int { return f * 2 } // C's RANK macro (for f <= 4)

// deflateOnce corresponds to one C deflate(strm, flush) call (wrap=0,
// avail_out always sufficient). Output goes to s.out (via the pending buffer).
func (s *state) deflateOnce(flush int) blockState {
	oldFlush := s.lastFlush
	s.lastFlush = flush

	// Nothing to do and a repeated/downgraded flush: C returns Z_BUF_ERROR and produces no output
	if s.availIn() == 0 && rank(flush) <= rank(oldFlush) && flush != zFinish {
		return needMore
	}

	if s.availIn() != 0 || s.lookahead != 0 ||
		(flush != zNoFlush && s.status != finishState) {
		var bs blockState
		if s.level == 0 {
			bs = deflateStored(s, flush)
		} else {
			bs = configurationTable[s.level].fn(s, flush)
		}
		if bs == finishStarted || bs == finishDone {
			s.status = finishState
		}
		if bs == needMore || bs == finishStarted {
			return bs
		}
		if bs == blockDone {
			if flush == zPartialFlush {
				s.trAlign()
			} else { // SYNC_FLUSH / FULL_FLUSH: write an empty stored block (sync marker)
				s.trStoredBlock(nil, false)
				if flush == zFullFlush {
					for i := range s.head { // forget history
						s.head[i] = nilPos
					}
					if s.lookahead == 0 {
						s.strstart = 0
						s.blockStart = 0
						s.insert = 0
					}
				}
			}
			s.flushPending()
		}
		return bs
	}
	if flush == zFinish {
		return finishDone // already in FINISH state: C returns Z_STREAM_END
	}
	return needMore
}

// readBuf reads at most size bytes from the input stream into dst
func (s *state) readBuf(dst []byte, size int) int {
	n := s.availIn()
	if n > size {
		n = size
	}
	if n == 0 {
		return 0
	}
	copy(dst[:n], s.in[s.inPos:s.inPos+n])
	s.inPos += n
	return n
}

// flushPending flushes the pending buffer to out (always fully flushed in one-shot mode)
func (s *state) flushPending() {
	s.biFlush()
	n := s.pending
	if n > s.availOut() {
		n = s.availOut()
	}
	if n == 0 {
		return
	}
	copy(s.out[s.outPos:], s.pendingBuf[s.pendingOut:s.pendingOut+n])
	s.outPos += n
	s.pendingOut += n
	s.pending -= n
	if s.pending == 0 {
		s.pendingOut = 0
	}
}

// updateHash corresponds to the UPDATE_HASH macro
func (s *state) updateHash(h uint32, c byte) uint32 {
	return ((h << hashShift) ^ uint32(c)) & hashMask
}

// insertString corresponds to the INSERT_STRING macro, returns the previous chain head (match_head)
func (s *state) insertString(str int) int {
	h := ((s.insH << hashShift) ^ uint32(s.window[str+minMatch-1])) & hashMask
	s.insH = h
	matchHead := s.head[h]
	s.prev[str&wMask] = matchHead
	s.head[h] = uint16(str)
	return int(matchHead)
}

// slideHash shifts the hash tables in step when the window slides down.
// Each entry becomes m >= wSize ? m-wSize : nilPos(0); wSize is 1<<15, so the
// condition is exactly the top bit of the 16-bit entry. (For prev, entries not
// on any hash chain are garbage but are never read, as in C.)
func (s *state) slideHash() {
	slideTable(s.head[:])
	slideTable(s.prev[:])
}

// slideTable applies the slide to every entry branch-free. C compilers
// vectorize the equivalent zlib loop into saturating-subtract SIMD; Go does
// not auto-vectorize, so when the table is 8-byte aligned the entries are
// processed four at a time as uint16 lanes of a uint64: lanes with the top
// bit set become lane-0x8000, all others become 0. Shifts and borrows cannot
// cross a lane (t has only bit 15 of each lane set), and lanes map to entries
// in memory order regardless of endianness, so the result is identical to the
// scalar loop. Both tables have a multiple-of-four length (wSize and hashSize).
func slideTable(tab []uint16) {
	if uintptr(unsafe.Pointer(&tab[0]))&7 == 0 {
		w := unsafe.Slice((*uint64)(unsafe.Pointer(&tab[0])), len(tab)/4)
		for i, v := range w {
			t := v & 0x8000800080008000
			w[i] = v & (t - t>>15)
		}
		return
	}
	for i, m := range tab {
		tab[i] = (m - wSize) & -(m >> 15)
	}
}

// fillWindow fills the window when the lookahead is insufficient
func (s *state) fillWindow() {
	for {
		more := windowSize - s.lookahead - s.strstart

		// If the window is almost full and the lookahead is insufficient, move the upper half to the lower half
		if s.strstart >= wSize+maxDistance {
			copy(s.window[0:], s.window[wSize:wSize+(wSize-more)])
			s.matchStart -= wSize
			s.strstart -= wSize // strstart is now >= MAX_DIST
			s.blockStart -= wSize
			if s.insert > s.strstart {
				s.insert = s.strstart
			}
			s.slideHash()
			more += wSize
		}
		if s.availIn() == 0 {
			break
		}

		n := s.readBuf(s.window[s.strstart+s.lookahead:], more)
		s.lookahead += n

		// Initialize the hash value now that we have some input
		if s.lookahead+s.insert >= minMatch {
			str := s.strstart - s.insert
			s.insH = uint32(s.window[str])
			s.insH = s.updateHash(s.insH, s.window[str+1])
			for s.insert != 0 {
				s.insH = s.updateHash(s.insH, s.window[str+minMatch-1])
				s.prev[str&wMask] = s.head[s.insH]
				s.head[s.insH] = uint16(str)
				str++
				s.insert--
				if s.lookahead+s.insert < minMatch {
					break
				}
			}
		}

		if !(s.lookahead < minLookahead && s.availIn() != 0) {
			break
		}
	}

	// Zero the never-written portion within WIN_INIT bytes past the end of the
	// data, so reads by longest_match past the data end are deterministic (as in C)
	if s.highWater < windowSize {
		curr := s.strstart + s.lookahead
		if s.highWater < curr {
			n := windowSize - curr
			if n > winInit {
				n = winInit
			}
			zeroRange(s.window[curr : curr+n])
			s.highWater = curr + n
		} else if s.highWater < curr+winInit {
			n := curr + winInit - s.highWater
			if n > windowSize-s.highWater {
				n = windowSize - s.highWater
			}
			zeroRange(s.window[s.highWater : s.highWater+n])
			s.highWater += n
		}
	}
}

func zeroRange(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// longestMatch finds the longest match starting at curMatch, sets matchStart and
// returns its length, at most lookahead. Port of the C version with wider loads:
// the byte loop is replaced by 8-byte XOR compares whose first differing byte
// (via TrailingZeros64) is exactly the byte the C loop stops at, and the four
// quick-reject byte compares become two 16-bit compares of the same bytes —
// the returned length, and therefore the compressed output, is unchanged.
func (s *state) longestMatch(curMatch int) int {
	chainLength := s.maxChainLength
	scan := s.strstart
	bestLen := s.prevLength // matches <= prevLength are not accepted
	niceMatch := s.niceMatch
	limit := 0
	if s.strstart > maxDistance {
		limit = s.strstart - maxDistance
	}
	win := s.window[:]
	prev := &s.prev

	// Same bytes as the C scan_end1/scan_end pair and the match head pair
	scanStart := binary.LittleEndian.Uint16(win[scan:])
	scanEnd := binary.LittleEndian.Uint16(win[scan+bestLen-1:])

	// Reduce the search effort when we already have a good match
	if s.prevLength >= s.goodMatch {
		chainLength >>= 2
	}
	// Do not search past the end of the input; required for deflate determinism
	if niceMatch > s.lookahead {
		niceMatch = s.lookahead
	}

	for {
		match := curMatch
		// Load the next chain link before the compares so the (serially
		// dependent) table read overlaps with the window reads below
		next := int(prev[match&wMask])

		// Quickly reject matches that cannot be longer: bytes match+bestLen-1,
		// match+bestLen, match, match+1 — as in C, offset 2 is implied by hash
		// equality once offsets 0 and 1 match
		if binary.LittleEndian.Uint16(win[match+bestLen-1:]) == scanEnd &&
			binary.LittleEndian.Uint16(win[match:]) == scanStart {

			// Compare offsets [3, maxMatch) eight bytes at a time; the last
			// group overlaps the previous one so reads end exactly at
			// scan+maxMatch, inside the window: strstart never exceeds
			// windowSize-minLookahead (zlib's "need lookahead" invariant), and
			// bytes past the input are zeroed by fillWindow up to highWater
			matchLen := maxMatch
			for j := 3; ; {
				x := binary.LittleEndian.Uint64(win[scan+j:]) ^
					binary.LittleEndian.Uint64(win[match+j:])
				if x != 0 {
					matchLen = j + bits.TrailingZeros64(x)>>3
					break
				}
				if j == maxMatch-8 {
					break
				}
				j += 8
				if j > maxMatch-8 {
					j = maxMatch - 8
				}
			}

			if matchLen > bestLen {
				s.matchStart = curMatch
				bestLen = matchLen
				if matchLen >= niceMatch {
					break
				}
				scanEnd = binary.LittleEndian.Uint16(win[scan+bestLen-1:])
			}
		}

		curMatch = next
		if curMatch <= limit {
			break
		}
		chainLength--
		if chainLength == 0 {
			break
		}
	}

	if bestLen <= s.lookahead {
		return bestLen
	}
	return s.lookahead
}

// flushBlockOnly corresponds to the FLUSH_BLOCK_ONLY macro
func (s *state) flushBlockOnly(last bool) {
	var buf []byte
	if s.blockStart >= 0 {
		buf = s.window[s.blockStart:]
	}
	s.trFlushBlock(buf, s.strstart-s.blockStart, last)
	s.blockStart = s.strstart
	s.flushPending()
}

// deflateStored, level 0: copy input directly as stored blocks whenever possible
func deflateStored(s *state, flush int) blockState {
	// Smallest block worth emitting when not flushing (32K with default parameters)
	minBlock := pendingBufSize - 5
	if wSize < minBlock {
		minBlock = wSize
	}

	last := false
	used := s.availIn()
	for {
		// len is the maximum block length that can be copied directly this pass
		length := maxStored
		have := (s.biValid + 42) >> 3 // number of header bytes
		if s.availOut() < have {
			break
		}
		have = s.availOut() - have
		left := s.strstart - s.blockStart // bytes left in the window
		if length > left+s.availIn() {
			length = left + s.availIn()
		}
		if length > have {
			length = have
		}

		// Block too small, or flushing but unable to copy all input: use the window and pending buffer instead
		if length < minBlock && ((length == 0 && flush != zFinish) ||
			flush == zNoFlush ||
			length != left+s.availIn()) {
			break
		}

		// First write a fake stored-block header in pending, then patch the length fields
		last = flush == zFinish && length == left+s.availIn()
		s.trStoredBlock(nil, last)

		s.pendingBuf[s.pending-4] = byte(length)
		s.pendingBuf[s.pending-3] = byte(length >> 8)
		s.pendingBuf[s.pending-2] = byte(^length)
		s.pendingBuf[s.pending-1] = byte(^length >> 8)

		s.flushPending()

		// Copy uncompressed bytes from the window
		if left > 0 {
			if left > length {
				left = length
			}
			copy(s.out[s.outPos:], s.window[s.blockStart:s.blockStart+left])
			s.outPos += left
			s.blockStart += left
			length -= left
		}

		// Copy directly from input to output
		if length > 0 {
			s.readBuf(s.out[s.outPos:s.outPos+length], length)
			s.outPos += length
		}
		if last {
			break
		}
	}

	// Update the sliding window with the last w_size bytes copied
	used -= s.availIn() // number of input bytes copied directly
	if used > 0 {
		if used >= wSize { // supplant the previous history entirely
			s.matches = 2 // marker to clear the hash table
			copy(s.window[0:wSize], s.in[s.inPos-wSize:s.inPos])
			s.strstart = wSize
			s.insert = s.strstart
		} else {
			if windowSize-s.strstart <= used {
				// slide the window down
				s.strstart -= wSize
				copy(s.window[0:], s.window[wSize:wSize+s.strstart])
				if s.matches < 2 {
					s.matches++ // add a pending slide_hash()
				}
				if s.insert > s.strstart {
					s.insert = s.strstart
				}
			}
			copy(s.window[s.strstart:], s.in[s.inPos-used:s.inPos])
			s.strstart += used
			s.insert += minInt(used, wSize-s.insert)
		}
		s.blockStart = s.strstart
	}
	if s.highWater < s.strstart {
		s.highWater = s.strstart
	}

	if last {
		return finishDone
	}

	// Done if flushing (non-FINISH) and the input is exhausted
	if flush != zNoFlush && flush != zFinish &&
		s.availIn() == 0 && s.strstart == s.blockStart {
		return blockDone
	}

	// Fill the window with any remaining input
	have := windowSize - s.strstart
	if s.availIn() > have && s.blockStart >= wSize {
		// slide the window down
		s.blockStart -= wSize
		s.strstart -= wSize
		copy(s.window[0:], s.window[wSize:wSize+s.strstart])
		if s.matches < 2 {
			s.matches++
		}
		have += wSize
		if s.insert > s.strstart {
			s.insert = s.strstart
		}
	}
	if have > s.availIn() {
		have = s.availIn()
	}
	if have > 0 {
		s.readBuf(s.window[s.strstart:], have)
		s.strstart += have
		s.insert += minInt(have, wSize-s.insert)
	}
	if s.highWater < s.strstart {
		s.highWater = s.strstart
	}

	// If there is not enough output space to emit a full block directly, write to the pending buffer instead
	have = (s.biValid + 42) >> 3
	have = minInt(pendingBufSize-have, maxStored)
	minBlock = minInt(have, wSize)
	left := s.strstart - s.blockStart
	if left >= minBlock ||
		((left > 0 || flush == zFinish) && flush != zNoFlush &&
			s.availIn() == 0 && left <= have) {
		length := minInt(left, have)
		last = flush == zFinish && s.availIn() == 0 && length == left
		s.trStoredBlock(s.window[s.blockStart:s.blockStart+length], last)
		s.blockStart += length
		s.flushPending()
	}

	if last {
		return finishStarted
	}
	return needMore
}

// deflateFast, levels 1-3: no lazy matching
func deflateFast(s *state, flush int) blockState {
	var hashHead int // head of the hash chain
	var bflush bool  // current block must be flushed

	for {
		// Make sure there is enough lookahead (unless at the end of input)
		if s.lookahead < minLookahead {
			s.fillWindow()
			if s.lookahead < minLookahead && flush == zNoFlush {
				return needMore
			}
			if s.lookahead == 0 {
				break // flush the current block
			}
		}

		// Insert window[strstart..strstart+2] into the dictionary
		hashHead = nilPos
		if s.lookahead >= minMatch {
			hashHead = s.insertString(s.strstart)
		}

		// Find the longest match (discarding those <= prevLength)
		if hashHead != nilPos && s.strstart-hashHead <= maxDistance {
			s.matchLength = s.longestMatch(hashHead)
		}
		if s.matchLength >= minMatch {
			bflush = s.tallyDist(s.strstart-s.matchStart, s.matchLength-minMatch)

			s.lookahead -= s.matchLength

			// Insert into the hash table position by position only if the match is not too long
			if s.matchLength <= s.maxLazyMatch /* max_insert_length */ &&
				s.lookahead >= minMatch {
				s.matchLength-- // string at strstart is already in the table
				for {
					s.strstart++
					s.insertString(s.strstart)
					s.matchLength--
					if s.matchLength == 0 {
						break
					}
				}
				s.strstart++
			} else {
				s.strstart += s.matchLength
				s.matchLength = 0
				s.insH = uint32(s.window[s.strstart])
				s.insH = s.updateHash(s.insH, s.window[s.strstart+1])
			}
		} else {
			// No match, emit a literal
			bflush = s.tallyLit(s.window[s.strstart])
			s.lookahead--
			s.strstart++
		}
		if bflush {
			s.flushBlockOnly(false)
			if s.availOut() == 0 {
				return needMore
			}
		}
	}
	s.insert = minInt(s.strstart, minMatch-1)
	if flush == zFinish {
		s.flushBlockOnly(true)
		if s.availOut() == 0 {
			return finishStarted
		}
		return finishDone
	}
	if s.symNext != 0 {
		s.flushBlockOnly(false)
		if s.availOut() == 0 {
			return needMore
		}
	}
	return blockDone
}

// deflateSlow, levels 4-9: lazy matching — the current match is only used
// if there is no better match at the next position
func deflateSlow(s *state, flush int) blockState {
	var hashHead int
	var bflush bool

	for {
		if s.lookahead < minLookahead {
			s.fillWindow()
			if s.lookahead < minLookahead && flush == zNoFlush {
				return needMore
			}
			if s.lookahead == 0 {
				break
			}
		}

		hashHead = nilPos
		if s.lookahead >= minMatch {
			hashHead = s.insertString(s.strstart)
		}

		// Find the longest match (discarding those <= prevLength)
		s.prevLength = s.matchLength
		s.prevMatch = s.matchStart
		s.matchLength = minMatch - 1

		if hashHead != nilPos && s.prevLength < s.maxLazyMatch &&
			s.strstart-hashHead <= maxDistance {
			s.matchLength = s.longestMatch(hashHead)

			// A length-3 match that is too distant is not worthwhile (Z_DEFAULT_STRATEGY)
			if s.matchLength <= 5 &&
				s.matchLength == minMatch && s.strstart-s.matchStart > tooFar {
				s.matchLength = minMatch - 1
			}
		}
		// The previous step had a match and the current one is no better: emit the previous match
		if s.prevLength >= minMatch && s.matchLength <= s.prevLength {
			maxInsert := s.strstart + s.lookahead - minMatch

			bflush = s.tallyDist(s.strstart-1-s.prevMatch, s.prevLength-minMatch)

			// Insert into the hash table all strings covered by the match
			s.lookahead -= s.prevLength - 1
			s.prevLength -= 2
			for {
				s.strstart++
				if s.strstart <= maxInsert {
					s.insertString(s.strstart)
				}
				s.prevLength--
				if s.prevLength == 0 {
					break
				}
			}
			s.matchAvailable = false
			s.matchLength = minMatch - 1
			s.strstart++

			if bflush {
				s.flushBlockOnly(false)
				if s.availOut() == 0 {
					return needMore
				}
			}
		} else if s.matchAvailable {
			// No match at the previous position (or it was truncated to a literal)
			bflush = s.tallyLit(s.window[s.strstart-1])
			if bflush {
				s.flushBlockOnly(false)
			}
			s.strstart++
			s.lookahead--
			if s.availOut() == 0 {
				return needMore
			}
		} else {
			// wait for the next step to decide
			s.matchAvailable = true
			s.strstart++
			s.lookahead--
		}
	}
	if s.matchAvailable {
		s.tallyLit(s.window[s.strstart-1])
		s.matchAvailable = false
	}
	s.insert = minInt(s.strstart, minMatch-1)
	if flush == zFinish {
		s.flushBlockOnly(true)
		if s.availOut() == 0 {
			return finishStarted
		}
		return finishDone
	}
	if s.symNext != 0 {
		s.flushBlockOnly(false)
		if s.availOut() == 0 {
			return needMore
		}
	}
	return blockDone
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
