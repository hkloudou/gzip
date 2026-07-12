//go:build cgo

package czlib

import (
	"bytes"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

// readRSS returns the current process RSS in bytes; linux only.
func readRSS(t *testing.T) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return pages * int64(os.Getpagesize())
}

// TestCgoMemoryBounded is a coarse-grained memory safety net for the cgo path:
// it exercises the main compression path heavily and creates/discards pooled
// streams (triggering finalizers that free C memory), and RSS growth must stay
// bounded. If the C-side output buffer leaks its free (~245KB × 2000 runs ≈
// 480MB) or the stream state leaks deflateEnd (measured ~262KB/stream × 1000
// ≈ 256MB), either would far exceed the 150MB threshold.
// Precise C-level leak checking is handled by the CI ASan/LSan job
// (native/leak_check.c).
func TestCgoMemoryBounded(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("RSS check runs on linux only")
	}
	if testing.Short() {
		t.Skip("skipped with -short")
	}

	data := bytes.Repeat([]byte(`{"leak":"check","payload":"0123456789abcdef"},`), 5000) // ~240KB

	// Warm up: let the pool, glibc arenas, and the Go heap reach steady state
	for i := 0; i < 50; i++ {
		if _, err := CompressOpts(data, 0, 1+i%9, 3); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	runtime.GC()
	debug.FreeOSMemory()
	base := readRSS(t)

	// 1) Heavy compression on the main path (pooled reuse + C output buffer freed each time)
	for i := 0; i < 2000; i++ {
		if _, err := CompressOpts(data, 0, 1+i%9, 3); err != nil {
			t.Fatal(err)
		}
	}
	// 2) Create/discard stream handles outside the pool: relies on finalizers
	// to free C state (measured ~262KB/stream, 1000 ≈ 256MB; a leak would
	// blow past the threshold)
	for i := 0; i < 1000; i++ {
		s, err := getCgoStream(6)
		if err != nil {
			t.Fatal(err)
		}
		_ = s // not Put back into the pool, just dropped → reclaimed by GC finalizer
		if i%50 == 0 {
			runtime.GC()
		}
	}
	// 3) Also exercise each test-only API once
	if _, err := CompressWithSyncFlush(data, 1, 6, 3, len(data)/2); err != nil {
		t.Fatal(err)
	}
	if _, err := CompressWithGzHeader(data, 6, 1, 3, []byte{1, 2}, "n", "c"); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	runtime.GC() // finalizers need two GC rounds
	debug.FreeOSMemory()
	after := readRSS(t)

	growth := after - base
	t.Logf("RSS: %d MB -> %d MB (growth %d MB)", base>>20, after>>20, growth>>20)
	// The threshold is deliberately loose (glibc may not return freed pages
	// to the kernel); it only catches unbounded leaks. Precise checking is
	// done by the ASan/LSan CI job.
	if growth > 150<<20 {
		t.Fatalf("RSS grew by %d MB, suspected cgo memory leak", growth>>20)
	}
}
