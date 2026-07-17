package gzip

// Streaming call-sequence cross-checks against real zlib 1.3.1 (gzip_ref
// referee, `stream` mode): the same (chunk, flush) deflate call sequence
// must produce byte-identical output on both sides. Ported from the former
// cgo-based internal/czlib stream tests.

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/hkloudou/gzip/internal/zdeflate"
)

// goDeflateRawStream replays a (chunk, flush) call sequence with the pure Go
// streaming implementation.
func goDeflateRawStream(t *testing.T, input []byte, level int, chunks []int, flushes []int) []byte {
	t.Helper()
	var d zdeflate.Deflater
	if err := d.Init(level); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var buf bytes.Buffer
	off := 0
	for i := range chunks {
		n := chunks[i]
		if off+n > len(input) {
			n = len(input) - off
		}
		if err := d.Deflate(input[off:off+n], flushes[i], &buf); err != nil {
			t.Fatal(err)
		}
		off += n
	}
	return buf.Bytes()
}

// TestStreamChunkedEqualsOneShot verifies zlib's determinism property (all
// levels): C and Go streaming output must be byte-identical; and for
// level>0, Z_NO_FLUSH streaming compression with arbitrary chunking ==
// one-shot compression.
// (At level 0, stored-block splitting depends on avail_in — C itself behaves
// this way — so we only require C↔Go equality, not equality with one-shot.)
func TestStreamChunkedEqualsOneShot(t *testing.T) {
	bin := needRef(t)
	rng := rand.New(rand.NewSource(7))
	data := make([]byte, 500*1024)
	for i := range data {
		data[i] = byte(rng.Intn(24))
	}

	for level := 0; level <= 9; level++ {
		oneShot := refDeflateRaw(t, bin, data, level)

		// Random chunking, all NO_FLUSH, FINISH at the end
		var chunks, flushes []int
		remain := len(data)
		for remain > 0 {
			n := 1 + rng.Intn(60000)
			if n > remain {
				n = remain
			}
			chunks = append(chunks, n)
			flushes = append(flushes, 0) // Z_NO_FLUSH
			remain -= n
		}
		flushes[len(flushes)-1] = 4 // Z_FINISH on the last chunk

		cOut := refStream(t, bin, data, level, chunks, flushes)
		gOut := goDeflateRawStream(t, data, level, chunks, flushes)

		if !bytes.Equal(cOut, gOut) {
			t.Errorf("level %d: C streaming != Go streaming (%d vs %d, first diff@%d)",
				level, len(cOut), len(gOut), firstDiff(cOut, gOut))
		}
		if level > 0 && !bytes.Equal(gOut, oneShot) {
			t.Errorf("level %d: streaming(NO_FLUSH) != one-shot (%d vs %d, first diff@%d)",
				level, len(gOut), len(oneShot), firstDiff(gOut, oneShot))
		}
	}
}

// TestStreamLevelFlushMatrix is an exhaustive hand-built matrix:
// levels 0-9 × flush types {PARTIAL, SYNC, FULL} × two chunking patterns,
// cross-checking real zlib against pure Go byte-by-byte for every combination.
func TestStreamLevelFlushMatrix(t *testing.T) {
	bin := needRef(t)
	rng := rand.New(rand.NewSource(11))
	data := make([]byte, 200*1024)
	for i := range data {
		data[i] = byte(rng.Intn(20))
	}
	n := len(data)

	flushNames := map[int]string{1: "PARTIAL_FLUSH", 2: "SYNC_FLUSH", 3: "FULL_FLUSH"}
	patterns := []struct {
		name    string
		chunks  []int
		flushes func(f int) []int
	}{
		// Three equal parts, the flush under test on the first two chunks
		{"thirds", []int{n / 3, n / 3, n - 2*(n/3)},
			func(f int) []int { return []int{f, f, 4} }},
		// Small head chunk + large tail chunk, mixed with NO_FLUSH
		{"head1k", []int{1024, 64 * 1024, n - 1024 - 64*1024},
			func(f int) []int { return []int{f, 0, 4} }},
	}

	for level := 0; level <= 9; level++ {
		for _, f := range []int{1, 2, 3} {
			for _, p := range patterns {
				flushes := p.flushes(f)
				cOut := refStream(t, bin, data, level, p.chunks, flushes)
				gOut := goDeflateRawStream(t, data, level, p.chunks, flushes)
				if !bytes.Equal(cOut, gOut) {
					t.Errorf("level=%d %s %s: mismatch (C=%d Go=%d first diff@%d)",
						level, flushNames[f], p.name, len(cOut), len(gOut), firstDiff(cOut, gOut))
				}
			}
		}
	}
}

// TestStreamSyncFlushCrossCheck cross-checks a set of fixed flush sequences,
// real zlib vs pure Go byte-by-byte.
func TestStreamSyncFlushCrossCheck(t *testing.T) {
	bin := needRef(t)
	rng := rand.New(rand.NewSource(9))
	data := make([]byte, 300*1024)
	for i := range data {
		data[i] = byte(rng.Intn(16))
	}

	cases := []struct {
		name    string
		chunks  []int
		flushes []int
	}{
		{"single_sync_mid", []int{150 * 1024, 150 * 1024}, []int{2, 4}},
		{"partial_flush", []int{100 * 1024, 200 * 1024}, []int{1, 4}},
		{"full_flush", []int{100 * 1024, 200 * 1024}, []int{3, 4}},
		{"many_syncs", []int{50 << 10, 50 << 10, 50 << 10, 50 << 10, 50 << 10, 50 << 10}, []int{2, 2, 2, 2, 2, 4}},
		{"sync_then_empty_sync", []int{100 * 1024, 0, 0, 200 * 1024}, []int{2, 2, 2, 4}},
		// Exact call sequence of gzip.Writer: Write; Flush; Write; Close
		{"writer_sequence", []int{150 * 1024, 0, 150 * 1024, 0}, []int{0, 2, 0, 4}},
		{"flush_at_start", []int{0, 300 * 1024}, []int{2, 4}},
		{"finish_only", []int{300 * 1024}, []int{4}},
	}

	for _, level := range []int{0, 1, 6, 9} {
		for _, c := range cases {
			cOut := refStream(t, bin, data, level, c.chunks, c.flushes)
			gOut := goDeflateRawStream(t, data, level, c.chunks, c.flushes)
			if !bytes.Equal(cOut, gOut) {
				t.Errorf("level %d %s: mismatch (C=%d Go=%d first diff@%d)",
					level, c.name, len(cOut), len(gOut), firstDiff(cOut, gOut))
			}
		}
	}
}

// TestStreamFuzz cross-checks random data × random chunking × random flush
// sequences × all levels.
func TestStreamFuzz(t *testing.T) {
	bin := needRef(t)
	iterations := 200
	if testing.Short() {
		iterations = 40
	}
	rng := rand.New(rand.NewSource(20260706))
	flushChoices := []int{0, 0, 0, 0, 1, 2, 2, 3} // mostly NO_FLUSH, mixed with each flush type

	for i := 0; i < iterations; i++ {
		size := rng.Intn(400 * 1024)
		alpha := 1 << (1 + rng.Intn(8))
		data := make([]byte, size)
		for j := range data {
			data[j] = byte(rng.Intn(alpha))
		}
		level := rng.Intn(10)

		var chunks, flushes []int
		remain := size
		for remain > 0 {
			n := rng.Intn(80000)
			if n > remain {
				n = remain
			}
			chunks = append(chunks, n)
			flushes = append(flushes, flushChoices[rng.Intn(len(flushChoices))])
			remain -= n
		}
		chunks = append(chunks, 0)
		flushes = append(flushes, 4) // Z_FINISH

		cOut := refStream(t, bin, data, level, chunks, flushes)
		gOut := goDeflateRawStream(t, data, level, chunks, flushes)
		if !bytes.Equal(cOut, gOut) {
			t.Fatalf("fuzz#%d: mismatch (size=%d alpha=%d level=%d ops=%d, C=%d Go=%d first diff@%d)",
				i, size, alpha, level, len(chunks), len(cOut), len(gOut), firstDiff(cOut, gOut))
		}
	}
}

// TestSyncFlushGzipCrossCheck cross-checks the full framed GZIP stream with
// a single mid-stream Z_SYNC_FLUSH: gzip.Writer (Write/Flush/Write/Close)
// vs the referee's compress mode with the same flush position.
func TestSyncFlushGzipCrossCheck(t *testing.T) {
	bin := needRef(t)
	data := bytes.Repeat([]byte(`{"id":1,"name":"user","email":"u@example.com"},`), 3000)
	for _, level := range []int{0, 1, 6, 9} {
		for _, split := range []int{0, 1, len(data) / 3, len(data) / 2, len(data) - 1, len(data)} {
			// At level 0 the Writer's NO_FLUSH+SYNC sequence and the
			// referee's single-SYNC call legitimately split stored blocks
			// differently (avail_in-dependent, same in C); the same-sequence
			// parity for level 0 is covered by TestStreamSyncFlushCrossCheck
			if level == 0 {
				continue
			}
			cOut := refGzip(t, bin, data, level, 1751038273, 3, split)

			var buf bytes.Buffer
			w, err := NewWriterLevel(&buf, level)
			if err != nil {
				t.Fatal(err)
			}
			w.Mtime = 1751038273
			if _, err := w.Write(data[:split]); err != nil {
				t.Fatal(err)
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(data[split:]); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(cOut, buf.Bytes()) {
				t.Errorf("level=%d split=%d: mismatch (C=%d Go=%d first diff@%d)",
					level, split, len(cOut), buf.Len(), firstDiff(cOut, buf.Bytes()))
			}
		}
	}
}
