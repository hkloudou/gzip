// Package gzip provides a GZIP compression API that is syntax-compatible
// with the standard library's compress/gzip, but whose compressed output is
// **byte-for-byte identical** to C zlib (the standard zlib used by iOS,
// Java, and most other languages).
//
// Migration (drop-in syntax):
//
//	import gzip "github.com/hkloudou/gzip"
//
//	func xx(data []byte) ([]byte, error) {
//		var buf bytes.Buffer
//		writer := gzip.NewWriter(&buf)
//		if _, err := writer.Write(data); err != nil {
//			return nil, err
//		}
//		// Close is required, otherwise the data is incomplete
//		if err := writer.Close(); err != nil {
//			return nil, err
//		}
//		return buf.Bytes(), nil
//	}
//
// Behavior is fully aligned with C zlib:
//   - Write is equivalent to deflate(Z_NO_FLUSH) — streaming incremental
//     compression that does not buffer the entire input; zlib guarantees
//     that under Z_NO_FLUSH the output is independent of how the Writes
//     are split (level>0), so as long as Flush is not called the output is
//     byte-identical to one-shot compression;
//   - Flush is equivalent to deflate(Z_SYNC_FLUSH) — like C zlib, it ends
//     the current deflate block and writes the 00 00 FF FF sync marker.
//     C zlib allows flushing mid-stream and so does this library, and for
//     the same call sequence the output is byte-identical to C;
//   - Close is equivalent to deflate(Z_FINISH) plus writing the GZIP
//     trailer.
//
// The header fields are identical to the standard library's Header
// (Comment/Extra/ModTime/Name/OS); writer.Name / writer.Comment /
// writer.Extra / writer.ModTime all migrate directly (they must be set
// before the first Write/Flush/Close).
//
// Differences from the standard library (to align the output with zlib):
//   - Compression levels match zlib: -1 = Z_DEFAULT_COMPRESSION (mapped to
//     6 internally by zlib; Java's Deflater.DEFAULT_COMPRESSION is also
//     -1), and 0-9; the standard-library-specific HuffmanOnly(-2) is not
//     supported;
//   - Header defaults are OS=3 (Unix), XFL=0, MTIME=0, matching zlib's
//     convention on Unix (the standard library defaults to OS=255 and
//     writes XFL=4/2 at levels 1/9);
//   - An extra writer.Mtime field is provided (the raw MTIME seconds as a
//     uint32; when non-zero it takes precedence over ModTime) — a design
//     unique to this library, giving precise timestamp control for
//     cross-language byte-for-byte comparisons.
//
// Decompression simply reuses the standard library (any valid GZIP stream
// can be decoded); this package forwards the same names, so
// NewReader/Reader are identical to the standard library.
package gzip

import (
	stdgzip "compress/gzip"
	"errors"
	"hash/crc32"
	"io"
	"time"

	"github.com/hkloudou/gzip/internal/zdeflate"
)

// Compression levels, identical to zlib.
const (
	NoCompression      = 0
	BestSpeed          = 1
	BestCompression    = 9
	DefaultCompression = -1 // zlib's Z_DEFAULT_COMPRESSION (internally equivalent to 6)
)

var (
	// ErrInvalidLevel is returned when the compression level is outside -1, 0-9.
	ErrInvalidLevel = errors.New("gzip: invalid compression level (want -1 or 0-9, same as zlib)")
	// ErrWriteAfterClose is returned when writing after Close.
	ErrWriteAfterClose = errors.New("gzip: write after close")
)

// Header is the configurable part of the GZIP file header. Its fields are
// identical to the standard library compress/gzip Header
// (Comment/Extra/ModTime/Name/OS), allowing seamless migration from the
// standard library. It must be set before the first Write/Flush/Close.
//
// Same as the standard library: Name/Comment are written per RFC 1952 as
// NUL-terminated Latin-1 (ISO 8859-1) strings, and a NUL or non-Latin-1
// character is an error; Extra is limited to 65535 bytes. With all fields
// at their zero values the header is byte-identical to earlier versions
// (FLG=0).
//
// Differences from the standard library (for byte-level parity with
// zlib/C implementations):
//   - OS defaults to 3 (Unix, the zlib convention); the standard library
//     defaults to 255;
//   - XFL is always 0 (the standard library writes 4/2 at levels 1/9);
//   - An extra Mtime field is provided — a design unique to this library:
//     it sets the raw MTIME seconds (uint32) of the GZIP header directly,
//     and when non-zero it takes precedence over ModTime. It is meant for
//     precise timestamp control in cross-language byte-for-byte
//     comparisons (bypassing time.Time's time zone/range conversions).
type Header struct {
	Comment string    // comment
	Extra   []byte    // "extra data"
	ModTime time.Time // modification time
	Name    string    // file name
	OS      byte      // operating system type

	// Mtime is specific to this library: it sets the MTIME seconds
	// directly; when non-zero it takes precedence over ModTime.
	Mtime uint32
}

// SetHeader copies all fields (Comment/Extra/ModTime/Name/OS) from the
// standard library's gzip.Header. Because this library's Header has the
// extra Mtime field, whole-struct assignment/conversion from the standard
// library Header does not compile (zw.Header = zr.Header) — use this
// method for scenarios such as recompression that need to carry the
// header over. Mtime is left unchanged.
func (z *Writer) SetHeader(h stdgzip.Header) {
	z.Comment = h.Comment
	z.Extra = h.Extra
	z.ModTime = h.ModTime
	z.Name = h.Name
	z.OS = h.OS
}

// Writer is an io.WriteCloser that streams a GZIP stream byte-identical to
// C zlib. Its semantics fully match C zlib: Write=Z_NO_FLUSH,
// Flush=Z_SYNC_FLUSH, Close=Z_FINISH. Not safe for concurrent use (same as
// the standard library).
type Writer struct {
	Header
	w           io.Writer
	level       int
	d           *zdeflate.Deflater
	wroteHeader bool
	closed      bool
	err         error
	crc         uint32
	size        uint32
}

// NewWriter creates a Writer with the default level (zlib level 6).
func NewWriter(w io.Writer) *Writer {
	z, _ := NewWriterLevel(w, DefaultCompression)
	return z
}

// NewWriterLevel creates a Writer with the given level. Levels match
// zlib: -1 means Z_DEFAULT_COMPRESSION (equivalent to 6), 0 is no
// compression (stored), 1 is fastest, 9 is best compression.
func NewWriterLevel(w io.Writer, level int) (*Writer, error) {
	if level != DefaultCompression && (level < 0 || level > 9) {
		return nil, ErrInvalidLevel
	}
	return &Writer{
		Header: Header{OS: 3},
		w:      w,
		level:  level,
	}, nil
}

// Reset resets the Writer for reuse (the level is kept; the header is
// reset to its defaults).
func (z *Writer) Reset(w io.Writer) {
	if z.d != nil {
		z.d.Close()
		z.d = nil
	}
	z.Header = Header{OS: 3}
	z.w = w
	z.wroteHeader = false
	z.closed = false
	z.err = nil
	z.crc = 0
	z.size = 0
}

// GZIP header FLG bits (RFC 1952 §2.3.1)
const (
	flagExtra   = 1 << 2 // FEXTRA
	flagName    = 1 << 3 // FNAME
	flagComment = 1 << 4 // FCOMMENT
)

// writeBytes writes a length prefix (LE uint16) followed by the content,
// used for the FEXTRA field. Semantics match the standard library
// compress/gzip.
func (z *Writer) writeBytes(b []byte) error {
	if len(b) > 0xffff {
		return errors.New("gzip: extra data is too large")
	}
	var buf [2]byte
	buf[0] = byte(len(b))
	buf[1] = byte(len(b) >> 8)
	if _, err := z.w.Write(buf[:]); err != nil {
		return err
	}
	_, err := z.w.Write(b)
	return err
}

// writeString writes a NUL-terminated Latin-1 (ISO 8859-1) string per
// RFC 1952, used for the FNAME/FCOMMENT fields. Semantics match the
// standard library compress/gzip: a NUL or non-Latin-1 character is an
// error.
func (z *Writer) writeString(s string) error {
	needconv := false
	for _, v := range s {
		if v == 0 || v > 0xff {
			return errors.New("gzip: non-Latin-1 header string")
		}
		if v > 0x7f {
			needconv = true
		}
	}
	var err error
	if needconv {
		b := make([]byte, 0, len(s))
		for _, v := range s {
			b = append(b, byte(v))
		}
		_, err = z.w.Write(b)
	} else {
		_, err = io.WriteString(z.w, s)
	}
	if err != nil {
		return err
	}
	// GZIP strings are NUL-terminated
	_, err = z.w.Write([]byte{0})
	return err
}

// ensureStarted writes the GZIP header (including the optional
// Extra/Name/Comment fields) and initializes the compressor.
func (z *Writer) ensureStarted() error {
	if z.wroteHeader {
		return nil
	}
	z.wroteHeader = true

	mtime := z.Mtime
	if mtime == 0 && z.ModTime.After(time.Unix(0, 0)) {
		// Same as the standard library: MTIME=0 means "not set";
		// times before 1970 are not written
		mtime = uint32(z.ModTime.Unix())
	}
	var flg byte
	if z.Extra != nil {
		flg |= flagExtra
	}
	if z.Name != "" {
		flg |= flagName
	}
	if z.Comment != "" {
		flg |= flagComment
	}
	// The fixed 10-byte part is byte-identical to the framing in
	// native/gzip_ref.cpp compress mode (XFL=0)
	hdr := [10]byte{
		0x1f, 0x8b, 0x08, flg,
		byte(mtime), byte(mtime >> 8), byte(mtime >> 16), byte(mtime >> 24),
		0x00, z.OS,
	}
	if _, err := z.w.Write(hdr[:]); err != nil {
		return err
	}
	// Optional fields in fixed order: FEXTRA, FNAME, FCOMMENT (RFC 1952)
	if z.Extra != nil {
		if err := z.writeBytes(z.Extra); err != nil {
			return err
		}
	}
	if z.Name != "" {
		if err := z.writeString(z.Name); err != nil {
			return err
		}
	}
	if z.Comment != "" {
		if err := z.writeString(z.Comment); err != nil {
			return err
		}
	}
	d, err := zdeflate.NewDeflater(z.level)
	if err != nil {
		return err
	}
	z.d = d
	return nil
}

// Write compresses p in streaming fashion (equivalent to C zlib's
// deflate(Z_NO_FLUSH)); the compressed data is written incrementally to
// the underlying Writer without buffering the entire input.
func (z *Writer) Write(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	if z.closed {
		return 0, ErrWriteAfterClose
	}
	if err := z.ensureStarted(); err != nil {
		z.err = err
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	z.crc = crc32.Update(z.crc, crc32.IEEETable, p)
	z.size += uint32(len(p))
	if err := z.d.Deflate(p, zdeflate.NoFlush, z.w); err != nil {
		z.err = err
		return 0, err
	}
	return len(p), nil
}

// Flush is equivalent to C zlib's deflate(Z_SYNC_FLUSH): it ends the
// current deflate block, writes the 00 00 FF FF sync marker, and pushes
// the compressed data to the underlying Writer.
// Calling it after Close returns nil (same as the standard library).
// Note: as with C zlib, where Flush is called changes the compressed byte
// stream (the decompressed result is unchanged); for the same call
// sequence the output is byte-identical to C zlib.
func (z *Writer) Flush() error {
	if z.err != nil {
		return z.err
	}
	if z.closed {
		return nil // same as the standard library: Flush after Close is a no-op
	}
	if err := z.ensureStarted(); err != nil {
		z.err = err
		return err
	}
	if err := z.d.Deflate(nil, zdeflate.SyncFlush, z.w); err != nil {
		z.err = err
		return err
	}
	return nil
}

// Close is equivalent to deflate(Z_FINISH): it ends the compressed stream
// and writes the 8-byte GZIP trailer.
// It does not close the underlying io.Writer.
func (z *Writer) Close() error {
	if z.err != nil {
		return z.err
	}
	if z.closed {
		return nil
	}
	z.closed = true
	if err := z.ensureStarted(); err != nil {
		z.err = err
		return err
	}
	if err := z.d.Deflate(nil, zdeflate.Finish, z.w); err != nil {
		z.err = err
		return err
	}
	z.d.Close()
	z.d = nil

	// GZIP Trailer (8 bytes)
	trailer := [8]byte{
		byte(z.crc), byte(z.crc >> 8), byte(z.crc >> 16), byte(z.crc >> 24),
		byte(z.size), byte(z.size >> 8), byte(z.size >> 16), byte(z.size >> 24),
	}
	if _, err := z.w.Write(trailer[:]); err != nil {
		z.err = err
		return err
	}
	return nil
}

/* ------------------------------------------------------------------ */
/* Decompression: forwarded directly to the standard library           */
/* (decompression has no byte-parity concerns)                         */
/* ------------------------------------------------------------------ */

// Reader is the standard library's gzip.Reader.
type Reader = stdgzip.Reader

var (
	// ErrChecksum is the same as the standard library's.
	ErrChecksum = stdgzip.ErrChecksum
	// ErrHeader is the same as the standard library's.
	ErrHeader = stdgzip.ErrHeader
)

// NewReader is identical to the standard library's gzip.NewReader.
func NewReader(r io.Reader) (*Reader, error) {
	return stdgzip.NewReader(r)
}
