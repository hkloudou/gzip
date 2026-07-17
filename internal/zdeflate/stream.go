// Streaming compression API: matches the call semantics of C zlib's
// deflate(strm, flush) — the same sequence of (input split, flush) calls
// produces byte-identical output to C zlib.
package zdeflate

import (
	"errors"
	"io"
)

// Flush constants, values identical to zlib. Z_PARTIAL_FLUSH (1) and
// Z_FULL_FLUSH (3) are equally valid Deflate inputs (the internal C port
// handles all five modes and the streaming cross-check matrix exercises
// them by value); only the modes the product Writer uses get named here.
const (
	NoFlush   = zNoFlush   // Z_NO_FLUSH: just feed data, no forced block boundary
	SyncFlush = zSyncFlush // Z_SYNC_FLUSH: end block + 00 00 FF FF marker
	Finish    = zFinish    // Z_FINISH: finalize the stream
)

var (
	ErrDeflaterClosed = errors.New("zdeflate: deflater closed")
	ErrAfterFinish    = errors.New("zdeflate: deflate called after finish")
	errInvalidFlush   = errors.New("zdeflate: invalid flush value")
)

// streamChunk: internal chunk size when level>0. Under Z_NO_FLUSH, zlib's
// output depends only on the data content and flush points, not on how
// avail_in is split (trailing lookahead < MIN_LOOKAHEAD is not processed
// under NO_FLUSH), so internal chunking does not affect the output bytes;
// it only lets us use a fixed-size output buffer. level=0 (stored) output
// depends on avail_in/avail_out, so to match C it must be processed as a
// whole, without chunking.
const streamChunk = 1 << 18 // 256KB

// Deflater is a streaming raw deflate compressor (windowBits=-15,
// memLevel=8, default strategy), corresponding to one long-lived z_stream.
// Use it as a value and (re)arm it with Init — the product Writer embeds
// one this way for zero per-stream allocations. Not safe for concurrent
// use.
type Deflater struct {
	s      *state
	closed bool
}

// Init (re)arms a Deflater for a new stream, taking compressor state from
// the pool. It may be called on a zero-value or previously Closed Deflater,
// letting callers embed one by value and reuse it with zero per-stream heap
// allocations. Calling Init on a live (non-closed) Deflater first releases
// its state, as Close would.
func (d *Deflater) Init(level int) error {
	if level == -1 {
		level = 6
	}
	if level < 0 || level > 9 {
		return errors.New("zdeflate: invalid compression level")
	}
	if !d.closed && d.s != nil {
		statePool.Put(d.s)
	}
	d.s = statePool.Get().(*state)
	d.s.reset(level)
	d.closed = false
	return nil
}

// Deflate is equivalent to C's deflate(strm, flush) (with avail_out always
// sufficient): it processes all bytes of p and writes the produced
// compressed data to w.
//
//   - flush=NoFlush: just feed data (corresponds to multiple Writes)
//   - flush=SyncFlush: end the current block and write the 00 00 FF FF sync marker
//   - flush=Finish: end the whole stream
//
// The same sequence of (p, flush) calls as C zlib yields byte-identical output.
func (d *Deflater) Deflate(p []byte, flush int, w io.Writer) error {
	if d.closed || d.s == nil { // nil s: zero value, needs Init first
		return ErrDeflaterClosed
	}
	if flush < NoFlush || flush > Finish {
		return errInvalidFlush
	}
	s := d.s
	if s.status == finishState && (len(p) > 0 || flush != Finish) {
		return ErrAfterFinish
	}

	for first := true; first || len(p) > 0; first = false {
		chunk := p
		// For level>0, split internally into streamChunk-sized pieces
		// (no effect on output, see the constant's comment)
		if s.level > 0 && len(chunk) > streamChunk {
			chunk = p[:streamChunk]
		}
		p = p[len(chunk):]

		f := flush
		if len(p) > 0 {
			f = NoFlush // only the last internal chunk applies the caller's flush
		}

		need := Bound(len(chunk)+2*wSize) + 64
		op := getScratch(need)
		out := *op
		s.in = chunk
		s.inPos = 0
		s.out = out
		s.outPos = 0

		// With sufficient avail_out a single deflateOnce consumes all
		// input; keep the progress check anyway to guard against data loss
		for {
			before := s.inPos
			s.deflateOnce(f)
			if s.availIn() == 0 {
				break
			}
			if s.inPos == before {
				s.in = nil
				s.out = nil
				putScratch(op)
				return errors.New("zdeflate: internal error: no progress")
			}
		}

		n := s.outPos
		s.in = nil
		s.out = nil
		if n > 0 {
			if _, err := w.Write(out[:n]); err != nil {
				putScratch(op)
				return err
			}
		}
		putScratch(op)
	}
	return nil
}

// Close releases the internal state (returns it to the pool). The Deflater
// must not be used afterwards (Init re-arms it). Safe on a zero-value or
// already-closed Deflater.
func (d *Deflater) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	s := d.s
	d.s = nil
	if s == nil {
		return nil
	}
	s.in = nil
	s.out = nil
	statePool.Put(s)
	return nil
}
