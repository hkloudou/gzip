package gzip

import (
	"bytes"
	stdgzip "compress/gzip"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hkloudou/gzip/internal/czlib"
)

// latin1Len returns the byte length of the string in Latin-1 encoding
// (one byte per rune).
func latin1Len(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// cXFL is the XFL byte that C zlib computes per level on the
// deflateSetHeader path (deflate.c: level==9→2, level<2→4, otherwise→0).
// This library's Writer always writes XFL=0.
func cXFL(level int) byte {
	if level == -1 {
		level = 6
	}
	switch {
	case level == 9:
		return 2
	case level < 2:
		return 4
	default:
		return 0
	}
}

// TestHeaderDefaultBytesUnchanged verifies the header output of a
// zero-value Header is byte-identical to earlier versions (FLG=0, XFL=0,
// OS=3), guaranteeing backward compatibility.
func TestHeaderDefaultBytesUnchanged(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Mtime = 0x68625E41
	w.Write([]byte("x"))
	w.Close()
	want := []byte{0x1f, 0x8b, 0x08, 0x00, 0x41, 0x5e, 0x62, 0x68, 0x00, 0x03}
	if !bytes.Equal(buf.Bytes()[:10], want) {
		t.Fatalf("default header changed:\n got % x\nwant % x", buf.Bytes()[:10], want)
	}
}

// TestHeaderStdlibRoundTrip sets all standard library fields, parses with
// the standard library Reader, and requires both fields and data to
// round-trip intact.
func TestHeaderStdlibRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("header round trip payload. "), 100)
	modTime := time.Unix(1751038273, 0)

	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Name = "data.json"
	w.Comment = "naïve café" // Latin-1, non-ASCII
	w.Extra = []byte{0xAB, 0x01, 0x04, 0x00, 0xde, 0xad, 0xbe, 0xef}
	w.ModTime = modTime
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := stdgzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "data.json" {
		t.Errorf("Name = %q", r.Name)
	}
	if r.Comment != "naïve café" {
		t.Errorf("Comment = %q", r.Comment)
	}
	if !bytes.Equal(r.Extra, []byte{0xAB, 0x01, 0x04, 0x00, 0xde, 0xad, 0xbe, 0xef}) {
		t.Errorf("Extra = % x", r.Extra)
	}
	if !r.ModTime.Equal(modTime) {
		t.Errorf("ModTime = %v, want %v", r.ModTime, modTime)
	}
	if r.OS != 3 {
		t.Errorf("OS = %d, want 3", r.OS)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("decompressed result mismatch")
	}
}

// TestHeaderPrefixMatchesStdlibWriter verifies that with identical fields
// the header bytes (including optional fields) exactly match the standard
// library Writer — at the default level both sides write XFL=0, so with
// the same explicit OS the header prefix can be compared byte-for-byte.
func TestHeaderPrefixMatchesStdlibWriter(t *testing.T) {
	name := "naïve.txt"
	comment := "café comment"
	extra := []byte{1, 2, 3, 4, 5}
	modTime := time.Unix(1751038273, 0)
	hdrLen := 10 + 2 + len(extra) + latin1Len(name) + 1 + latin1Len(comment) + 1

	var ours, std bytes.Buffer

	w := NewWriter(&ours)
	w.Name = name
	w.Comment = comment
	w.Extra = extra
	w.ModTime = modTime
	w.OS = 3
	w.Write([]byte("z"))
	w.Close()

	sw := stdgzip.NewWriter(&std) // default level: stdlib XFL=0
	sw.Name = name
	sw.Comment = comment
	sw.Extra = extra
	sw.ModTime = modTime
	sw.OS = 3
	sw.Write([]byte("z"))
	sw.Close()

	if !bytes.Equal(ours.Bytes()[:hdrLen], std.Bytes()[:hdrLen]) {
		t.Fatalf("header differs from the standard library:\n got % x\nwant % x",
			ours.Bytes()[:hdrLen], std.Bytes()[:hdrLen])
	}
}

// TestHeaderMatchesCZlib cross-checks the complete GZIP output
// byte-for-byte against real C zlib (deflateSetHeader, windowBits=15+16).
// The only known difference is the XFL byte (C computes it per level,
// this library always writes 0); it is corrected with C's formula before
// comparing.
//
// Note: the payload is deliberately kept within a single stored block
// (<32KB) — level 0 stored-block splitting depends on the call sequence
// (the Writer does NO_FLUSH+FINISH, the C reference a single FINISH);
// with a small payload the two sequences produce identical output, which
// lets level 0 take part in the byte-for-byte comparison. Level 0
// sequence semantics for large payloads are covered by the zlib package's
// streaming cross-checks and crossnative.
func TestHeaderMatchesCZlib(t *testing.T) {
	if !czlib.HasCGO() {
		t.Skip("requires CGO_ENABLED=1 (real C zlib)")
	}
	payload := bytes.Repeat([]byte(`{"k":"v","header":"cross-check"},`), 500)
	ts := uint32(1751038273)

	cases := []struct {
		label   string
		name    string
		comment string
		extra   []byte
	}{
		{"name-only", "file.bin", "", nil},
		{"comment-only", "", "a comment", nil},
		{"extra-only", "", "", []byte{0x01, 0x02, 0x00, 0xff}},
		{"empty-extra", "", "", []byte{}}, // non-nil empty Extra: FEXTRA + xlen=0
		{"all", "naïve.json", "café ×÷", []byte{0xde, 0xad}},
		{"none", "", "", nil},
	}
	for _, c := range cases {
		for _, hOS := range []byte{3, 255} {
			for _, hTS := range []uint32{0, ts} {
				for level := -1; level <= 9; level++ {
					want, err := czlib.CompressWithGzHeader(payload, level, hTS, hOS, c.extra, c.name, c.comment)
					if err != nil {
						t.Fatalf("%s level %d: C: %v", c.label, level, err)
					}

					var buf bytes.Buffer
					w, err := NewWriterLevel(&buf, level)
					if err != nil {
						t.Fatal(err)
					}
					w.Mtime = hTS
					w.OS = hOS
					w.Name = c.name
					w.Comment = c.comment
					w.Extra = c.extra
					if _, err := w.Write(payload); err != nil {
						t.Fatal(err)
					}
					if err := w.Close(); err != nil {
						t.Fatal(err)
					}

					got := append([]byte(nil), buf.Bytes()...)
					if got[8] != 0 {
						t.Errorf("%s level %d: Writer XFL should always be 0, got %#x", c.label, level, got[8])
					}
					got[8] = cXFL(level) // correct the known XFL difference
					if !bytes.Equal(got, want) {
						off := firstDiff(got, want)
						t.Errorf("%s os=%d ts=%d level %d: differs from C zlib (len %d vs %d, first diff @%d)",
							c.label, hOS, hTS, level, len(got), len(want), off)
					}
				}
			}
		}
	}
}

// TestHeaderFuzzMatchesCZlib randomizes every header parameter at once —
// Latin-1 names/comments (high bytes, length edges up to 1KB), Extra
// (absent / empty-present / random up to the 65535 boundary), OS and MTIME
// extremes, levels -1..9, mixed payloads — and requires the Writer's output
// to be byte-identical to real C zlib's deflateSetHeader (XFL fixed up),
// then round-trips the decoded fields through the standard library reader.
// Fixed seed; ZLIB_FUZZ_ITER / ZLIB_FUZZ_SEED adjust intensity.
func TestHeaderFuzzMatchesCZlib(t *testing.T) {
	if !czlib.HasCGO() {
		t.Skip("requires CGO_ENABLED=1 (real C zlib)")
	}
	iterations := 150
	if testing.Short() {
		iterations = 25
	}
	seed := int64(20260712)
	if v := os.Getenv("ZLIB_FUZZ_ITER"); v != "" {
		fmt.Sscanf(v, "%d", &iterations)
	}
	if v := os.Getenv("ZLIB_FUZZ_SEED"); v != "" {
		fmt.Sscanf(v, "%d", &seed)
	}
	rng := rand.New(rand.NewSource(seed))

	// any rune in [1,255] is valid Latin-1 for a header string (0 terminates)
	latin1 := func(n int) string {
		r := make([]rune, n)
		for i := range r {
			r[i] = rune(1 + rng.Intn(255))
		}
		return string(r)
	}
	strLens := []int{0, 0, 1, 2, 7, 40, 255, 1024}
	extraLens := []int{-1, -1, 0, 1, 4, 100, 4096, 65535} // -1: nil (no FEXTRA)
	mtimes := []uint32{0, 1, 0x7FFFFFFF, 0xFFFFFFFF}
	oses := []byte{0, 3, 11, 255}

	for i := 0; i < iterations; i++ {
		name := latin1(strLens[rng.Intn(len(strLens))])
		comment := latin1(strLens[rng.Intn(len(strLens))])
		var extra []byte
		if el := extraLens[rng.Intn(len(extraLens))]; el >= 0 {
			extra = make([]byte, el)
			rng.Read(extra) // Extra may contain any byte, including NUL
		}
		ts := mtimes[rng.Intn(len(mtimes))]
		if rng.Intn(2) == 0 {
			ts = rng.Uint32()
		}
		osByte := oses[rng.Intn(len(oses))]
		if rng.Intn(2) == 0 {
			osByte = byte(rng.Intn(256))
		}
		level := rng.Intn(11) - 1

		// payload < 32KB so at level 0 the Writer's NO_FLUSH+FINISH call
		// sequence produces the same stored-block split as the one-shot C
		// path (see TestHeaderMatchesCZlib)
		payload := make([]byte, rng.Intn(8192))
		alpha := []int{2, 16, 256}[rng.Intn(3)]
		for j := range payload {
			payload[j] = byte(rng.Intn(alpha))
		}

		want, err := czlib.CompressWithGzHeader(payload, level, ts, osByte, extra, name, comment)
		if err != nil {
			t.Fatalf("fuzz#%d: C zlib: %v", i, err)
		}

		var buf bytes.Buffer
		w, err := NewWriterLevel(&buf, level)
		if err != nil {
			t.Fatal(err)
		}
		w.Mtime = ts
		w.OS = osByte
		w.Name = name
		w.Comment = comment
		w.Extra = extra
		if _, err := w.Write(payload); err != nil {
			t.Fatalf("fuzz#%d: %v", i, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("fuzz#%d: %v", i, err)
		}

		got := append([]byte(nil), buf.Bytes()...)
		if got[8] != 0 {
			t.Fatalf("fuzz#%d: Writer XFL should always be 0, got %#x", i, got[8])
		}
		got[8] = cXFL(level)
		if !bytes.Equal(got, want) {
			off := firstDiff(got, want)
			t.Fatalf("fuzz#%d: differs from C zlib (name=%dB comment=%dB extra=%dB(nil=%v) ts=%d os=%d level=%d payload=%dB; len %d vs %d, first diff @%d)",
				i, latin1Len(name), latin1Len(comment), len(extra), extra == nil, ts, osByte, level, len(payload), len(got), len(want), off)
		}

		// independent check: the standard library must decode the same fields.
		// stdlib's reader rejects NUL-terminated header strings longer than
		// 511 bytes (its internal buffer); the gzip spec has no such limit —
		// the 1024-byte length edge is covered by the C comparison above.
		if latin1Len(name) > 511 || latin1Len(comment) > 511 {
			continue
		}
		r, err := stdgzip.NewReader(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("fuzz#%d: stdlib reader: %v", i, err)
		}
		back, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("fuzz#%d: stdlib read: %v", i, err)
		}
		if !bytes.Equal(back, payload) {
			t.Fatalf("fuzz#%d: payload round-trip mismatch", i)
		}
		if r.Header.Name != name || r.Header.Comment != comment {
			t.Fatalf("fuzz#%d: name/comment did not survive the round-trip", i)
		}
		if len(r.Header.Extra) != len(extra) || (len(extra) > 0 && !bytes.Equal(r.Header.Extra, extra)) {
			t.Fatalf("fuzz#%d: extra did not survive the round-trip (%dB vs %dB)", i, len(r.Header.Extra), len(extra))
		}
		if err := r.Close(); err != nil {
			t.Fatalf("fuzz#%d: %v", i, err)
		}
	}
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// TestHeaderMtimePriority verifies a non-zero Mtime takes precedence over
// ModTime (a design unique to this library); a ModTime before 1970 writes
// 0, same as the standard library.
func TestHeaderMtimePriority(t *testing.T) {
	read := func(hdr []byte) uint32 {
		return uint32(hdr[4]) | uint32(hdr[5])<<8 | uint32(hdr[6])<<16 | uint32(hdr[7])<<24
	}

	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Mtime = 42
	w.ModTime = time.Unix(1751038273, 0)
	w.Write([]byte("x"))
	w.Close()
	if got := read(buf.Bytes()); got != 42 {
		t.Errorf("Mtime did not take precedence: MTIME=%d, want 42", got)
	}

	buf.Reset()
	w = NewWriter(&buf)
	w.ModTime = time.Unix(-1000, 0) // before 1970
	w.Write([]byte("x"))
	w.Close()
	if got := read(buf.Bytes()); got != 0 {
		t.Errorf("ModTime before 1970 should write 0, got %d", got)
	}
}

// TestHeaderInvalidStrings verifies that non-Latin-1 or NUL-containing
// Name/Comment values return an error, matching standard library
// semantics.
func TestHeaderInvalidStrings(t *testing.T) {
	for _, bad := range []string{"Ωmega", "nul\x00byte", "emoji😀"} {
		var buf bytes.Buffer
		w := NewWriter(&buf)
		w.Name = bad
		if _, err := w.Write([]byte("x")); err == nil {
			t.Errorf("Name=%q should return an error", bad)
		}

		buf.Reset()
		w = NewWriter(&buf)
		w.Comment = bad
		if err := w.Close(); err == nil {
			t.Errorf("Comment=%q should return an error", bad)
		}
	}
}

// TestHeaderExtraTooLarge verifies an Extra over 65535 bytes returns an
// error (same as the standard library).
func TestHeaderExtraTooLarge(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Extra = make([]byte, 0x10000)
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("a 65536-byte Extra should return an error")
	} else if !strings.Contains(err.Error(), "extra data is too large") {
		t.Fatalf("error message %q does not match the standard library", err)
	}
}

// TestHeaderEmptyExtraPresent verifies a non-nil empty Extra still writes
// FEXTRA (xlen=0), matching the standard library's `Extra != nil` check.
func TestHeaderEmptyExtraPresent(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Extra = []byte{}
	w.Write([]byte("x"))
	w.Close()
	if buf.Bytes()[3]&0x04 == 0 {
		t.Fatal("non-nil empty Extra did not set the FEXTRA bit")
	}
	r, err := stdgzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
}

// TestSetHeader carries header fields over from a standard library Reader
// (whole-struct assignment does not compile because of the Mtime field;
// SetHeader is the substitute).
func TestSetHeader(t *testing.T) {
	var src bytes.Buffer
	w1 := NewWriter(&src)
	w1.Name = "orig.bin"
	w1.Comment = "meta"
	w1.Extra = []byte{9, 8}
	w1.ModTime = time.Unix(1751038273, 0)
	w1.Write([]byte("payload"))
	w1.Close()

	r, err := NewReader(bytes.NewReader(src.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(r)

	var dst bytes.Buffer
	w2 := NewWriter(&dst)
	w2.SetHeader(r.Header)
	if w2.Name != "orig.bin" || w2.Comment != "meta" ||
		!bytes.Equal(w2.Extra, []byte{9, 8}) ||
		!w2.ModTime.Equal(time.Unix(1751038273, 0)) {
		t.Fatalf("SetHeader did not copy fields correctly: %+v", w2.Header)
	}
	// The OS parsed by the standard library Reader must pass through too
	if w2.OS != 3 {
		t.Fatalf("SetHeader OS = %d", w2.OS)
	}
}

// TestFlushAfterCloseReturnsNil verifies Flush after Close returns nil
// (same as the standard library).
func TestFlushAfterCloseReturnsNil(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush after Close should return nil (same as stdlib), got %v", err)
	}
	// Write still errors (same stdlib semantics: writing to a closed stream is an error)
	if _, err := w.Write([]byte("y")); err == nil {
		t.Fatal("Write after Close should return an error")
	}
}

// TestHeaderResetClears verifies header fields return to their defaults
// after Reset.
func TestHeaderResetClears(t *testing.T) {
	var a, b bytes.Buffer
	w := NewWriter(&a)
	w.Name = "old"
	w.Comment = "old"
	w.Extra = []byte{1}
	w.Write([]byte("x"))
	w.Close()

	w.Reset(&b)
	w.Write([]byte("x"))
	w.Close()
	if b.Bytes()[3] != 0 {
		t.Fatalf("FLG=%#x after Reset, want 0", b.Bytes()[3])
	}
	if b.Bytes()[9] != 3 {
		t.Fatalf("OS=%d after Reset, want 3", b.Bytes()[9])
	}
}
