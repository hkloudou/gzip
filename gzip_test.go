package gzip

import (
	"bytes"
	"encoding/base64"
	"errors"
	"hash/crc32"
	"io"
	"testing"

	"github.com/hkloudou/gzip/internal/czlib"
	"github.com/hkloudou/gzip/internal/zdeflate"
)

// xx is the user-requested stdlib-syntax migration example, copied
// verbatim from compress/gzip usage.
func xx(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := NewWriter(&buf)
	// writer.
	_, err := writer.Write(data)
	if err != nil {
		return nil, err
	}

	// Close is required, otherwise the data is incomplete
	if err := writer.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func TestStdlibSyntaxMigration(t *testing.T) {
	out, err := xx([]byte("hello hello hello world"))
	if err != nil {
		t.Fatal(err)
	}
	// Verify by decompressing (standard library Reader)
	r, err := NewReader(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello hello hello world" {
		t.Fatalf("decompressed result mismatch: %q", got)
	}
}

// TestGoldenViaWriter reproduces the golden vectors with Writer + Mtime
// (byte-identical to C zlib).
func TestGoldenViaWriter(t *testing.T) {
	cases := []struct {
		input    string
		ts       uint32
		expected string
	}{
		{"{}", 1751038273, "H4sIAEG5XmgAA6uuBQBDv6ajAgAAAA=="},
		{`{"access_token":"eyJhbGciOiJIUzUxMiJ9.eyJsb2dpbl91c2VyX2tleSI6IjA4N2M2N2E1MGVkNjQwOWY5MzZjMzU3OTdiOTU3ZmFjIn0.4HTb_NXUmYMNf6sJhJbPzZdUtEvV-g0IcKM_OaJl74XaFofsq9_W1MPvPjoxz-Fd_x_WEsotPz7MjUqf_5Uwng"}`,
			1751038275,
			"H4sIAEO5XmgAAw3IwQ6CIBgA4HfxXkuymt06aEH7wZaoeWGCWpJpDVdG693rO34fp1CqMkYM/bXqnLVTvclFblXDGoK55SM0xJ/+00hU3mXruwol7wwNbXXES6w3HkWAKApc2CZXqg8vlp4WYHMNls9ZXDYs5vP8FmrczabeLpaCZvx2AlovDbkQGdm85EPwTCbnGVZ7EKwg7crLirCvzcMXqQvRM9L9aCdhKUaRBqYfIrsCzR+1WPBXd3a+P72ZqCnGAAAA"},
	}
	for i, c := range cases {
		var buf bytes.Buffer
		w := NewWriter(&buf)
		w.Mtime = c.ts
		if _, err := w.Write([]byte(c.input)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		got := base64.StdEncoding.EncodeToString(buf.Bytes())
		if got != c.expected {
			t.Errorf("case %d: golden mismatch\n got: %s\nwant: %s", i, got, c.expected)
		}
	}
}

// TestWriterMatchesOneShot verifies the Writer output is byte-identical
// to the one-shot reference implementation (the pure-Go reference always;
// under CGO builds it is additionally cross-checked against real C zlib).
func TestWriterMatchesOneShot(t *testing.T) {
	data := bytes.Repeat([]byte(`{"key":"value","n":42},`), 1000)
	ts := uint32(1751038273)

	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Mtime = ts
	w.Write(data)
	w.Close()

	want := zdeflate.GzipCompressLevel(data, ts, -1, 3)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("Writer output differs from the pure-Go one-shot reference (%d vs %d bytes)", buf.Len(), len(want))
	}
	if czlib.HasCGO() {
		cWant, err := czlib.CompressOpts(data, ts, -1, 3)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), cWant) {
			t.Fatal("Writer output differs from real C zlib")
		}
	}
}

// TestWriterLevels verifies every level works, decompresses, and matches
// the one-shot reference implementation.
func TestWriterLevels(t *testing.T) {
	data := bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog. "), 200)
	for level := 0; level <= 9; level++ {
		var buf bytes.Buffer
		w, err := NewWriterLevel(&buf, level)
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		w.Write(data)
		if err := w.Close(); err != nil {
			t.Fatalf("level %d: %v", level, err)
		}

		// Cross-check against the one-shot reference (mtime=0; under CGO
		// builds additionally against real C zlib)
		want := zdeflate.GzipCompressLevel(data, 0, level, 3)
		if !bytes.Equal(buf.Bytes(), want) {
			t.Errorf("level %d: Writer output differs from the pure-Go one-shot reference", level)
		}
		if czlib.HasCGO() {
			cWant, err := czlib.CompressOpts(data, 0, level, 3)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(buf.Bytes(), cWant) {
				t.Errorf("level %d: Writer output differs from real C zlib", level)
			}
		}

		// Verify by decompressing
		r, err := NewReader(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("level %d: decompressed result mismatch", level)
		}
	}

	// Invalid levels
	for _, level := range []int{-2, 10, 100} {
		if _, err := NewWriterLevel(io.Discard, level); err == nil {
			t.Errorf("level %d should return an error", level)
		}
	}
}

// TestWriterEmpty verifies empty input still produces a valid GZIP stream
// (matching standard library behavior).
func TestWriterEmpty(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %d bytes", len(got))
	}
}

// TestWriterChunkedWrites verifies multiple Writes produce the same
// output as a single Write.
func TestWriterChunkedWrites(t *testing.T) {
	data := bytes.Repeat([]byte("chunked write test data 1234567890"), 5000)

	var one bytes.Buffer
	w1 := NewWriter(&one)
	w1.Write(data)
	w1.Close()

	var many bytes.Buffer
	w2 := NewWriter(&many)
	for i := 0; i < len(data); i += 777 {
		end := i + 777
		if end > len(data) {
			end = len(data)
		}
		w2.Write(data[i:end])
	}
	w2.Close()

	if !bytes.Equal(one.Bytes(), many.Bytes()) {
		t.Fatal("chunked writes and a single write produced different output")
	}
}

// TestWriterFlushMatchesZlib verifies Flush (Z_SYNC_FLUSH) is
// byte-identical to the one-shot reference for the same call sequence
// (under CGO builds the reference is real C zlib).
func TestWriterFlushMatchesZlib(t *testing.T) {
	data := bytes.Repeat([]byte(`{"id":1,"name":"user","email":"u@example.com"},`), 2000)
	ts := uint32(1751038273)
	split := len(data) / 2

	want := syncFlushReference(t, data, ts, -1, split)
	if czlib.HasCGO() {
		cWant, err := czlib.CompressWithSyncFlush(data, ts, -1, 3, split)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(cWant, want) {
			t.Fatal("pure-Go sync flush reference differs from real C zlib")
		}
	}

	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.Mtime = ts
	if _, err := w.Write(data[:split]); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil { // = C zlib deflate(Z_SYNC_FLUSH)
		t.Fatal(err)
	}
	if _, err := w.Write(data[split:]); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("Writer with Flush differs from C zlib for the same call sequence (%d vs %d)", buf.Len(), len(want))
	}

	// Sync marker is present and the stream decompresses
	if !bytes.Contains(buf.Bytes(), []byte{0x00, 0x00, 0xff, 0xff}) {
		t.Fatal("Z_SYNC_FLUSH 00 00 FF FF marker not found")
	}
	r, err := NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("decompressed result mismatch")
	}
}

// TestWriterAllLevelsWithFlush exercises the full matrix by hand:
// levels -1,0-9 × a mid-stream Flush.
//
// The Writer's call sequence is deflate(a,NO_FLUSH); deflate(SYNC);
// deflate(b,NO_FLUSH); deflate(FINISH). Every level is cross-checked
// against a low-level replay of the exact same call sequence; for level>0
// it is additionally cross-checked against the single-SYNC semantics
// reference with deflate(a,SYNC) semantics (the two are byte-identical
// when level>0; at level 0 stored-block splitting depends on avail_in,
// and in C zlib the two sequences inherently differ by one empty block —
// level 0 C-vs-Go same-sequence parity is covered by internal/czlib's
// streaming cross-check tests).
func TestWriterAllLevelsWithFlush(t *testing.T) {
	data := bytes.Repeat([]byte(`{"level":%d,"payload":"0123456789abcdef"},`), 3000)
	ts := uint32(1751038273)
	split := len(data) / 2

	for level := -1; level <= 9; level++ {
		var buf bytes.Buffer
		w, err := NewWriterLevel(&buf, level)
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		w.Mtime = ts
		w.Write(data[:split])
		if err := w.Flush(); err != nil {
			t.Fatalf("level %d: Flush: %v", level, err)
		}
		w.Write(data[split:])
		if err := w.Close(); err != nil {
			t.Fatalf("level %d: Close: %v", level, err)
		}

		// Low-level replay of the same call sequence (available in any build mode)
		want := replaySequence(t, data, ts, level, split)
		if !bytes.Equal(buf.Bytes(), want) {
			t.Errorf("level %d: Writer (with Flush) differs from the same-call-sequence replay (%d vs %d)",
				level, buf.Len(), len(want))
		}

		// level>0: NO_FLUSH+SYNC is byte-identical to a single SYNC, so
		// also cross-check against the single-SYNC semantics reference
		// (under CGO builds additionally against real C zlib)
		if level != 0 {
			want2 := syncFlushReference(t, data, ts, level, split)
			if !bytes.Equal(buf.Bytes(), want2) {
				t.Errorf("level %d: Writer (with Flush) differs from the single-SYNC semantics reference (%d vs %d)",
					level, buf.Len(), len(want2))
			}
			if czlib.HasCGO() {
				want3, err := czlib.CompressWithSyncFlush(data, ts, level, 3, split)
				if err != nil {
					t.Fatalf("level %d: %v", level, err)
				}
				if !bytes.Equal(buf.Bytes(), want3) {
					t.Errorf("level %d: Writer (with Flush) differs from real C zlib", level)
				}
			}
		}

		r, err := NewReader(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("level %d: decompressed result mismatch", level)
		}
	}
}

// replaySequence replays the Writer's exact call sequence (NO_FLUSH,
// SYNC, NO_FLUSH, FINISH) with the low-level zdeflate API and applies the
// same GZIP framing.
func replaySequence(t *testing.T, data []byte, ts uint32, level, split int) []byte {
	t.Helper()
	d, err := zdeflate.NewDeflater(level)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var raw bytes.Buffer
	if err := d.Deflate(data[:split], zdeflate.NoFlush, &raw); err != nil {
		t.Fatal(err)
	}
	if err := d.Deflate(nil, zdeflate.SyncFlush, &raw); err != nil {
		t.Fatal(err)
	}
	if err := d.Deflate(data[split:], zdeflate.NoFlush, &raw); err != nil {
		t.Fatal(err)
	}
	if err := d.Deflate(nil, zdeflate.Finish, &raw); err != nil {
		t.Fatal(err)
	}

	out := make([]byte, 0, 18+raw.Len())
	out = append(out, 0x1f, 0x8b, 0x08, 0x00,
		byte(ts), byte(ts>>8), byte(ts>>16), byte(ts>>24), 0x00, 0x03)
	out = append(out, raw.Bytes()...)
	crc := crc32.ChecksumIEEE(data)
	n := uint32(len(data))
	out = append(out,
		byte(crc), byte(crc>>8), byte(crc>>16), byte(crc>>24),
		byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
	return out
}

// syncFlushReference replays the single-SYNC semantics with zdeflate
// (deflate(a, Z_SYNC_FLUSH); deflate(b, Z_FINISH)) and applies GZIP
// framing (mtime=ts, OS=3). Byte-identical to C zlib for the same call
// sequence (guaranteed by this file's czlib cross-check legs and the
// crossnative three-way comparison).
func syncFlushReference(t *testing.T, data []byte, ts uint32, level, split int) []byte {
	t.Helper()
	d, err := zdeflate.NewDeflater(level)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var raw bytes.Buffer
	if err := d.Deflate(data[:split], zdeflate.SyncFlush, &raw); err != nil {
		t.Fatal(err)
	}
	if err := d.Deflate(data[split:], zdeflate.Finish, &raw); err != nil {
		t.Fatal(err)
	}

	out := make([]byte, 0, 18+raw.Len())
	out = append(out, 0x1f, 0x8b, 0x08, 0x00,
		byte(ts), byte(ts>>8), byte(ts>>16), byte(ts>>24), 0x00, 0x03)
	out = append(out, raw.Bytes()...)
	crc := crc32.ChecksumIEEE(data)
	n := uint32(len(data))
	out = append(out,
		byte(crc), byte(crc>>8), byte(crc>>16), byte(crc>>24),
		byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
	return out
}

// TestWriterMultiFlush verifies multiple-Flush semantics (including
// consecutive Flushes and a Flush with no data): same as C zlib — a
// repeated Flush with no new data produces no extra output.
func TestWriterMultiFlush(t *testing.T) {
	for _, level := range []int{0, 1, 6, 9} {
		var buf bytes.Buffer
		w, _ := NewWriterLevel(&buf, level)
		w.Write([]byte("part one, "))
		w.Flush()
		sizeAfterFirst := buf.Len()
		w.Flush() // no new data: like C, this must produce no output
		if buf.Len() != sizeAfterFirst {
			t.Errorf("level %d: repeated Flush produced extra output (%d -> %d)", level, sizeAfterFirst, buf.Len())
		}
		w.Write([]byte("part two, "))
		w.Flush()
		w.Write([]byte("part three"))
		if err := w.Close(); err != nil {
			t.Fatalf("level %d: %v", level, err)
		}

		r, err := NewReader(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		if string(got) != "part one, part two, part three" {
			t.Errorf("level %d: decompressed result %q", level, got)
		}
	}
}

// TestWriterFlushDataAvailable verifies data is decodable immediately
// after Flush (streaming semantics).
func TestWriterFlushDataAvailable(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	msg := []byte("hello streaming world")
	w.Write(msg)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	// Not Closed yet, but flushed data must already be fully decodable
	r, err := NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	r.Multistream(false)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("read %q after Flush, want %q", got, msg)
	}
	w.Close()
}

// TestWriterReset reuses the Writer.
func TestWriterReset(t *testing.T) {
	var a, b bytes.Buffer
	w := NewWriter(&a)
	w.Write([]byte("first"))
	w.Close()

	w.Reset(&b)
	w.Write([]byte("first"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("output differs after Reset")
	}

	// Writing after Close returns ErrWriteAfterClose, detectable via errors.Is
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrWriteAfterClose) {
		t.Fatalf("Write after Close should return ErrWriteAfterClose, got %v", err)
	}
}
