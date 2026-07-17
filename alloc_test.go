package gzip

import (
	"bytes"
	"io"
	"testing"

	"github.com/hkloudou/gzip/internal/zdeflate"
)

// TestSteadyStateAllocs pins the steady-state per-op allocation counts of
// the pooled compression paths. Any upward drift here means a pooling
// regression: state or scratch buffers escaping reuse and accumulating —
// the Go-side equivalent of a leak (the C referee side is swept by
// ASan/LSan in CI). The expected constants:
//
//   - one-shot GzipCompressLevel: exactly 1 allocation, the returned
//     exact-size result slice (compressor state and scratch are pooled);
//   - a full Writer cycle (Reset + Write + Flush + Close into io.Discard):
//     0 allocations — the Deflater is embedded by value and re-armed via
//     Init, and header/trailer bytes go through the Writer's wbuf scratch
//     field instead of escaping locals.
//
// testing.AllocsPerRun averages over many runs, so a rare pool refill
// after a GC adds far less than the 0.2 slack.
func TestSteadyStateAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under the race detector")
	}
	data := bytes.Repeat([]byte(`{"key":"value","n":12345},`), 200) // ~5KB, compressible

	// Warm the pools (state + scratch).
	for i := 0; i < 8; i++ {
		zdeflate.GzipCompressLevel(data, 0, 6, 3)
	}
	oneShot := testing.AllocsPerRun(200, func() {
		zdeflate.GzipCompressLevel(data, 0, 6, 3)
	})
	if oneShot > 1.2 {
		t.Errorf("one-shot steady state allocates %.2f/op, want 1 (result slice only) — pooling regression?", oneShot)
	}

	w := NewWriter(io.Discard)
	cycle := func() {
		w.Reset(io.Discard)
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		cycle()
	}
	writerCycle := testing.AllocsPerRun(200, cycle)
	if writerCycle > 0.2 {
		t.Errorf("Writer Reset/Write/Flush/Close steady state allocates %.2f/op, want 0 — pooling regression?", writerCycle)
	}
}
