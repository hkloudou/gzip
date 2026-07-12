//go:build cgo
// +build cgo

package czlib

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/hkloudou/gzip/internal/zdeflate"
)

// goDeflateRawStream replays the same (chunk, flush) call sequence using the pure Go streaming implementation.
func goDeflateRawStream(input []byte, level int, chunks []uint32, flushes []int32) ([]byte, error) {
	d, err := zdeflate.NewDeflater(level)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	var buf bytes.Buffer
	off := 0
	for i := range chunks {
		n := int(chunks[i])
		if off+n > len(input) {
			n = len(input) - off
		}
		if err := d.Deflate(input[off:off+n], int(flushes[i]), &buf); err != nil {
			return nil, err
		}
		off += n
	}
	return buf.Bytes(), nil
}

// TestStreamChunkedEqualsOneShot verifies zlib's determinism property (all levels):
// C and Go streaming output must be byte-identical; and for level>0,
// Z_NO_FLUSH streaming compression with arbitrary chunking == one-shot
// compression.
// (At level 0, stored-block splitting depends on avail_in — C itself behaves
// this way — so we only require C↔Go equality, not equality with one-shot.)
func TestStreamChunkedEqualsOneShot(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	data := make([]byte, 500*1024)
	for i := range data {
		data[i] = byte(rng.Intn(24))
	}

	for level := 0; level <= 9; level++ {
		oneShot, err := cgoDeflateRaw(data, level)
		if err != nil {
			t.Fatal(err)
		}

		// Random chunking, all NO_FLUSH, FINISH at the end
		var chunks []uint32
		var flushes []int32
		remain := len(data)
		for remain > 0 {
			n := 1 + rng.Intn(60000)
			if n > remain {
				n = remain
			}
			chunks = append(chunks, uint32(n))
			flushes = append(flushes, 0) // Z_NO_FLUSH
			remain -= n
		}
		flushes[len(flushes)-1] = 4 // Z_FINISH on the last chunk

		cOut, err := cgoDeflateRawStream(data, level, chunks, flushes)
		if err != nil {
			t.Fatal(err)
		}
		gOut, err := goDeflateRawStream(data, level, chunks, flushes)
		if err != nil {
			t.Fatal(err)
		}

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
// cross-checking C zlib against pure Go byte-by-byte for every combination.
func TestStreamLevelFlushMatrix(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	data := make([]byte, 200*1024)
	for i := range data {
		data[i] = byte(rng.Intn(20))
	}
	n := uint32(len(data))

	flushNames := map[int32]string{1: "PARTIAL_FLUSH", 2: "SYNC_FLUSH", 3: "FULL_FLUSH"}
	patterns := []struct {
		name    string
		chunks  []uint32
		flushes func(f int32) []int32
	}{
		// Three equal parts, the flush under test on the first two chunks
		{"thirds", []uint32{n / 3, n / 3, n - 2*(n/3)},
			func(f int32) []int32 { return []int32{f, f, 4} }},
		// Small head chunk + large tail chunk, mixed with NO_FLUSH
		{"head1k", []uint32{1024, 64 * 1024, n - 1024 - 64*1024},
			func(f int32) []int32 { return []int32{f, 0, 4} }},
	}

	for level := 0; level <= 9; level++ {
		for _, f := range []int32{1, 2, 3} {
			for _, p := range patterns {
				flushes := p.flushes(f)
				cOut, err := cgoDeflateRawStream(data, level, p.chunks, flushes)
				if err != nil {
					t.Fatalf("level=%d %s %s: C failed: %v", level, flushNames[f], p.name, err)
				}
				gOut, err := goDeflateRawStream(data, level, p.chunks, flushes)
				if err != nil {
					t.Fatalf("level=%d %s %s: Go failed: %v", level, flushNames[f], p.name, err)
				}
				if !bytes.Equal(cOut, gOut) {
					t.Errorf("level=%d %s %s: mismatch (C=%d Go=%d first diff@%d)",
						level, flushNames[f], p.name, len(cOut), len(gOut), firstDiff(cOut, gOut))
				}
			}
		}
	}
}

// TestStreamSyncFlushCrossCheck cross-checks a set of fixed flush sequences, C vs Go byte-by-byte.
func TestStreamSyncFlushCrossCheck(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	data := make([]byte, 300*1024)
	for i := range data {
		data[i] = byte(rng.Intn(16))
	}

	cases := []struct {
		name    string
		chunks  []uint32
		flushes []int32
	}{
		{"single_sync_mid", []uint32{150 * 1024, 150 * 1024}, []int32{2, 4}},
		{"partial_flush", []uint32{100 * 1024, 200 * 1024}, []int32{1, 4}},
		{"full_flush", []uint32{100 * 1024, 200 * 1024}, []int32{3, 4}},
		{"many_syncs", []uint32{50 << 10, 50 << 10, 50 << 10, 50 << 10, 50 << 10, 50 << 10}, []int32{2, 2, 2, 2, 2, 4}},
		{"sync_then_empty_sync", []uint32{100 * 1024, 0, 0, 200 * 1024}, []int32{2, 2, 2, 4}},
		// Exact call sequence of gzip.Writer: Write; Flush; Write; Close
		{"writer_sequence", []uint32{150 * 1024, 0, 150 * 1024, 0}, []int32{0, 2, 0, 4}},
		{"flush_at_start", []uint32{0, 300 * 1024}, []int32{2, 4}},
		{"finish_only_empty", []uint32{300 * 1024}, []int32{4}},
	}

	for _, level := range []int{0, 1, 6, 9} {
		for _, c := range cases {
			cOut, err := cgoDeflateRawStream(data, level, c.chunks, c.flushes)
			if err != nil {
				t.Fatalf("level %d %s: C failed: %v", level, c.name, err)
			}
			gOut, err := goDeflateRawStream(data, level, c.chunks, c.flushes)
			if err != nil {
				t.Fatalf("level %d %s: Go failed: %v", level, c.name, err)
			}
			if !bytes.Equal(cOut, gOut) {
				t.Errorf("level %d %s: mismatch (C=%d Go=%d first diff@%d)",
					level, c.name, len(cOut), len(gOut), firstDiff(cOut, gOut))
			}
		}
	}
}

// TestStreamFuzz cross-checks random data × random chunking × random flush sequences × all levels.
func TestStreamFuzz(t *testing.T) {
	iterations := 200
	if testing.Short() {
		iterations = 40
	}
	rng := rand.New(rand.NewSource(20260706))
	flushChoices := []int32{0, 0, 0, 0, 1, 2, 2, 3} // mostly NO_FLUSH, mixed with each flush type

	for i := 0; i < iterations; i++ {
		size := rng.Intn(400 * 1024)
		alpha := 1 << (1 + rng.Intn(8))
		data := make([]byte, size)
		for j := range data {
			data[j] = byte(rng.Intn(alpha))
		}
		level := rng.Intn(10)

		var chunks []uint32
		var flushes []int32
		remain := size
		for remain > 0 {
			n := rng.Intn(80000)
			if n > remain {
				n = remain
			}
			chunks = append(chunks, uint32(n))
			flushes = append(flushes, flushChoices[rng.Intn(len(flushChoices))])
			remain -= n
		}
		chunks = append(chunks, 0)
		flushes = append(flushes, 4) // Z_FINISH

		cOut, err := cgoDeflateRawStream(data, level, chunks, flushes)
		if err != nil {
			t.Fatalf("fuzz#%d: C failed: %v", i, err)
		}
		gOut, err := goDeflateRawStream(data, level, chunks, flushes)
		if err != nil {
			t.Fatalf("fuzz#%d: Go failed: %v", i, err)
		}
		if !bytes.Equal(cOut, gOut) {
			t.Fatalf("fuzz#%d: mismatch (size=%d alpha=%d level=%d ops=%d, C=%d Go=%d first diff@%d)",
				i, size, alpha, level, len(chunks), len(cOut), len(gOut), firstDiff(cOut, gOut))
		}
	}
}

// TestSyncFlushGzipCrossCheck cross-checks CompressWithSyncFlush, C vs pure Go.
func TestSyncFlushGzipCrossCheck(t *testing.T) {
	data := bytes.Repeat([]byte(`{"id":1,"name":"user","email":"u@example.com"},`), 3000)
	for _, level := range []int{0, 1, 6, 9} {
		for _, split := range []int{0, 1, len(data) / 3, len(data) / 2, len(data) - 1, len(data)} {
			cOut, err := CompressWithSyncFlush(data, 1751038273, level, 3, split)
			if err != nil {
				t.Fatal(err)
			}
			gOut := goSyncFlushGzip(data, 1751038273, level, 3, split)
			if !bytes.Equal(cOut, gOut) {
				t.Errorf("level=%d split=%d: mismatch (C=%d Go=%d first diff@%d)",
					level, split, len(cOut), len(gOut), firstDiff(cOut, gOut))
			}
		}
	}
}

// goSyncFlushGzip replays the single-SYNC semantics with the pure Go
// implementation and wraps the result in a GZIP frame, matching the semantics
// of CompressWithSyncFlush (real C zlib).
func goSyncFlushGzip(data []byte, ts uint32, level int, osByte byte, split int) []byte {
	if split < 0 {
		split = 0
	}
	if split > len(data) {
		split = len(data)
	}
	d, err := zdeflate.NewDeflater(level)
	if err != nil {
		panic(err)
	}
	defer d.Close()
	var raw bytes.Buffer
	if err := d.Deflate(data[:split], zdeflate.SyncFlush, &raw); err != nil {
		panic(err)
	}
	if err := d.Deflate(data[split:], zdeflate.Finish, &raw); err != nil {
		panic(err)
	}
	return gzipFrameOpts(raw.Bytes(), data, ts, osByte)
}
