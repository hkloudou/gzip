// This file corresponds to zlib's deflate.c (fixed windowBits=15 / memLevel=8 /
// Z_DEFAULT_STRATEGY, i.e. the default zlib parameters in the iOS/Java ecosystems).
package zdeflate

import (
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

	// Symbol storage: layout selected at build time (sym_split.go is the
	// default, speed-first; sym_lowmem.go behind the gziplowmem tag is
	// C's pending_buf overlay, 48KB smaller per state). Both record the
	// identical (dist, lc) stream with identical flush points, so the
	// output bytes are the same — the LIT_MEM decision record lives in
	// CLAUDE.md with the measured numbers.
	symArea
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

// hash3 is C's rolling ins_h at position str, computed directly from the
// three bytes it covers instead of by rolling. The two are equal at every
// position: hashShift*minMatch == hashBits (5*3 == 15), so on each rolling
// step the oldest byte's contribution is shifted entirely above hashMask,
// leaving ins_h(str) == (w[str]<<10 ^ w[str+1]<<5 ^ w[str+2]) & hashMask
// regardless of prior history (bits already above the mask can only shift
// further out, so intermediate masking changes nothing). The indices are
// masked with windowSize-1 purely so the compiler drops the bounds checks:
// batch-insert positions always satisfy str+2 < windowSize (they lie at
// least minMatch bytes inside the valid window data), so the masks are
// identity and the loaded bytes are unchanged.
func (s *state) hash3(str int) uint32 {
	return (uint32(s.window[str&(windowSize-1)])<<(2*hashShift) ^
		uint32(s.window[(str+1)&(windowSize-1)])<<hashShift ^
		uint32(s.window[(str+2)&(windowSize-1)])) & hashMask
}

// insertPos inserts position str into the hash chains with an independently
// computed hash — the chain updates are identical to INSERT_STRING because
// hash3(str) == C's rolled ins_h at str (see hash3); the match_head return
// is not needed at the batch call sites (C discards it there too). Unlike
// insertString it does not touch s.insH: the batch loops that use it restore
// insH afterwards to exactly the value C's rolling loop would leave.
func (s *state) insertPos(str int) {
	h := s.hash3(str)
	s.prev[str&wMask] = s.head[h]
	s.head[h] = uint16(str)
}

// insertRun performs insertPos(str) for str = first..last (inclusive,
// no-op when last < first) — deflateSlow's proven-hot batch-insert loop,
// selected on arm64 only (wideInsertRun, see insert_arm64.go for the A/B
// data). Instead of three window byte loads per position,
// one 8-byte little-endian load sources six consecutive hashes: byte k of
// load64w(&s.window, str) is window[str+k] (both load flavors are
// little-endian), so each hK below is literally hash3(str+k) — the same
// three bytes, the same shifts, the same mask. Chain updates issue in
// ascending position order exactly like the scalar loop, so when two
// positions share a hash bucket the later prev link still reads the earlier
// insert's head entry: every head/prev value stored is byte-identical to
// calling insertPos in order. Positions beyond wideEnd fall back to
// insertPos itself.
//
// Bounds: lane k uses window bytes str+k..str+k+2, and str+5 <= last keeps
// every used lane at a position <= last, whose bytes are valid window data
// at every call site (each inserted position satisfies str+2 < valid end,
// as for insertPos). The load touches window[str..str+7], so the wide loop
// also stops at windowSize-8 (the masked fast-flavor load would otherwise
// run past the array and the portable flavor would slice-panic); the scalar
// tail covers the last few positions near the window end.
func (s *state) insertRun(first, last int) {
	str := first
	// 12-position iterations first: two independent 8-byte loads (x covers
	// lanes 0-5, y = load at str+6 covers lanes 6-11 the same way), giving
	// the core two parallel extract chains and half the loop overhead on
	// the ~250-position runs compressible data produces. Same hashes, same
	// ascending store order as twelve insertPos calls — the y lanes are
	// literally hash3(str+6+k). Memory bound: the second load needs
	// str+6 <= windowSize-8; value bound: lane 11 needs str+11 <= last.
	wideEnd12 := last - 11
	if wideEnd12 > windowSize-14 {
		wideEnd12 = windowSize - 14
	}
	for str <= wideEnd12 {
		x := load64w(&s.window, str)
		y := load64w(&s.window, str+6)
		xb0 := uint32(x) & 0xff
		xb1 := uint32(x>>8) & 0xff
		xb2 := uint32(x>>16) & 0xff
		xb3 := uint32(x>>24) & 0xff
		xb4 := uint32(x>>32) & 0xff
		xb5 := uint32(x>>40) & 0xff
		xb6 := uint32(x>>48) & 0xff
		xb7 := uint32(x >> 56)
		yb0 := uint32(y) & 0xff
		yb1 := uint32(y>>8) & 0xff
		yb2 := uint32(y>>16) & 0xff
		yb3 := uint32(y>>24) & 0xff
		yb4 := uint32(y>>32) & 0xff
		yb5 := uint32(y>>40) & 0xff
		yb6 := uint32(y>>48) & 0xff
		yb7 := uint32(y >> 56)
		h0 := (xb0<<(2*hashShift) ^ xb1<<hashShift ^ xb2) & hashMask
		h1 := (xb1<<(2*hashShift) ^ xb2<<hashShift ^ xb3) & hashMask
		h2 := (xb2<<(2*hashShift) ^ xb3<<hashShift ^ xb4) & hashMask
		h3 := (xb3<<(2*hashShift) ^ xb4<<hashShift ^ xb5) & hashMask
		h4 := (xb4<<(2*hashShift) ^ xb5<<hashShift ^ xb6) & hashMask
		h5 := (xb5<<(2*hashShift) ^ xb6<<hashShift ^ xb7) & hashMask
		h6 := (yb0<<(2*hashShift) ^ yb1<<hashShift ^ yb2) & hashMask
		h7 := (yb1<<(2*hashShift) ^ yb2<<hashShift ^ yb3) & hashMask
		h8 := (yb2<<(2*hashShift) ^ yb3<<hashShift ^ yb4) & hashMask
		h9 := (yb3<<(2*hashShift) ^ yb4<<hashShift ^ yb5) & hashMask
		h10 := (yb4<<(2*hashShift) ^ yb5<<hashShift ^ yb6) & hashMask
		h11 := (yb5<<(2*hashShift) ^ yb6<<hashShift ^ yb7) & hashMask
		s.prev[str&wMask] = s.head[h0]
		s.head[h0] = uint16(str)
		s.prev[(str+1)&wMask] = s.head[h1]
		s.head[h1] = uint16(str + 1)
		s.prev[(str+2)&wMask] = s.head[h2]
		s.head[h2] = uint16(str + 2)
		s.prev[(str+3)&wMask] = s.head[h3]
		s.head[h3] = uint16(str + 3)
		s.prev[(str+4)&wMask] = s.head[h4]
		s.head[h4] = uint16(str + 4)
		s.prev[(str+5)&wMask] = s.head[h5]
		s.head[h5] = uint16(str + 5)
		s.prev[(str+6)&wMask] = s.head[h6]
		s.head[h6] = uint16(str + 6)
		s.prev[(str+7)&wMask] = s.head[h7]
		s.head[h7] = uint16(str + 7)
		s.prev[(str+8)&wMask] = s.head[h8]
		s.head[h8] = uint16(str + 8)
		s.prev[(str+9)&wMask] = s.head[h9]
		s.head[h9] = uint16(str + 9)
		s.prev[(str+10)&wMask] = s.head[h10]
		s.head[h10] = uint16(str + 10)
		s.prev[(str+11)&wMask] = s.head[h11]
		s.head[h11] = uint16(str + 11)
		str += 12
	}
	wideEnd := last - 5
	if wideEnd > windowSize-8 {
		wideEnd = windowSize - 8
	}
	for str <= wideEnd {
		x := load64w(&s.window, str)
		b0 := uint32(x) & 0xff
		b1 := uint32(x>>8) & 0xff
		b2 := uint32(x>>16) & 0xff
		b3 := uint32(x>>24) & 0xff
		b4 := uint32(x>>32) & 0xff
		b5 := uint32(x>>40) & 0xff
		b6 := uint32(x>>48) & 0xff
		b7 := uint32(x >> 56)
		h0 := (b0<<(2*hashShift) ^ b1<<hashShift ^ b2) & hashMask
		h1 := (b1<<(2*hashShift) ^ b2<<hashShift ^ b3) & hashMask
		h2 := (b2<<(2*hashShift) ^ b3<<hashShift ^ b4) & hashMask
		h3 := (b3<<(2*hashShift) ^ b4<<hashShift ^ b5) & hashMask
		h4 := (b4<<(2*hashShift) ^ b5<<hashShift ^ b6) & hashMask
		h5 := (b5<<(2*hashShift) ^ b6<<hashShift ^ b7) & hashMask
		s.prev[str&wMask] = s.head[h0]
		s.head[h0] = uint16(str)
		s.prev[(str+1)&wMask] = s.head[h1]
		s.head[h1] = uint16(str + 1)
		s.prev[(str+2)&wMask] = s.head[h2]
		s.head[h2] = uint16(str + 2)
		s.prev[(str+3)&wMask] = s.head[h3]
		s.head[h3] = uint16(str + 3)
		s.prev[(str+4)&wMask] = s.head[h4]
		s.head[h4] = uint16(str + 4)
		s.prev[(str+5)&wMask] = s.head[h5]
		s.head[h5] = uint16(str + 5)
		str += 6
	}
	for ; str <= last; str++ {
		s.insertPos(str)
	}
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
// scalar loop. On arm64 (wideSlideTable, see tune_arm64.go for the A/B
// data) the word loop handles two words per iteration — the pairs are
// disjoint and each word gets the exact same transform, so this changes
// only the loop overhead, not a single stored value; the tail loop covers
// a trailing odd word (never taken today: both tables are 8192 words —
// wSize/4 and hashSize/4 — but kept so no future table length can silently
// skip an entry). Every entry is written exactly once either way.
func slideTable(tab []uint16) {
	if uintptr(unsafe.Pointer(&tab[0]))&7 == 0 {
		w := unsafe.Slice((*uint64)(unsafe.Pointer(&tab[0])), len(tab)/4)
		if wideSlideTable {
			i := 0
			for ; i+1 < len(w); i += 2 {
				v0, v1 := w[i], w[i+1]
				t0 := v0 & 0x8000800080008000
				t1 := v1 & 0x8000800080008000
				w[i] = v0 & (t0 - t0>>15)
				w[i+1] = v1 & (t1 - t1>>15)
			}
			for ; i < len(w); i++ {
				v := w[i]
				t := v & 0x8000800080008000
				w[i] = v & (t - t>>15)
			}
			return
		}
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
	win := &s.window
	prev := &s.prev

	// Same bytes as the C scan_end1/scan_end pair and the match head pair
	scanStart := load16m(win, scan)
	scanEnd := load16m(win, scan+bestLen-1)

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
		if load16m(win, match+bestLen-1) == scanEnd &&
			load16m(win, match) == scanStart {

			// Compare offsets [3, maxMatch) eight bytes at a time; the last
			// group overlaps the previous one so reads end exactly at
			// scan+maxMatch, inside the window: strstart never exceeds
			// windowSize-minLookahead (zlib's "need lookahead" invariant), and
			// bytes past the input are zeroed by fillWindow up to highWater
			matchLen := maxMatch
			for j := 3; ; {
				x := load64w(win, scan+j) ^ load64w(win, match+j)
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
				scanEnd = load16m(win, scan+bestLen-1)
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
				// Positions strstart+1 .. strstart+matchLength-1 (the string
				// at strstart is already in the table) — exactly the set C's
				// rolling loop inserts. insertPos computes each hash
				// independently so consecutive inserts do not serialize on
				// ins_h; insH is then set to hash3(last), the value C's
				// ins_h holds after its loop, so the rolling path continues
				// identically. matchLength >= minMatch here, so the loop
				// always inserts at least one position. (insertRun is not
				// used here: matchLength <= maxLazyMatch <= 6 at levels 1-3
				// keeps every run under insertRun's 6-position wide chunk,
				// so it could only ever take insertRun's scalar tail.)
				last := s.strstart + s.matchLength - 1
				for str := s.strstart + 1; str <= last; str++ {
					s.insertPos(str)
				}
				s.insH = s.hash3(last)
				s.strstart = last + 1
				s.matchLength = 0
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

			// Insert into the hash table all strings covered by the match:
			// positions strstart+1 .. strstart+prevLength-2, capped at
			// maxInsert — exactly the set C's rolling loop inserts. Both
			// arms compute each hash independently so the inserts do not
			// serialize on ins_h; insH is then set to hash3(last inserted),
			// the value C's ins_h holds after its loop. If every position
			// was skipped (last < first), C leaves ins_h untouched and so
			// does this path. wideInsertRun is a compile-time constant
			// (tune_arm64.go / tune_other.go, chosen from A/B CI data —
			// including the closed x86 threshold experiments recorded
			// there), so each build keeps exactly one arm; insertRun is
			// byte-identical to the scalar loop either way.
			s.lookahead -= s.prevLength - 1
			end := s.strstart + s.prevLength - 2
			first := s.strstart + 1
			last := end
			if last > maxInsert {
				last = maxInsert
			}
			if wideInsertRun {
				s.insertRun(first, last)
			} else {
				for str := first; str <= last; str++ {
					s.insertPos(str)
				}
			}
			if last >= first {
				s.insH = s.hash3(last)
			}
			s.strstart = end
			s.prevLength = 0
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
