package gzip

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"sync"
	"testing"

	"github.com/hkloudou/gzip/internal/zdeflate"
)

// sync.Pool is the easiest way to introduce silent data corruption, so the
// pooled compressor state (zdeflate statePool) and scratch buffers
// (scratchPool) get dedicated adversarial coverage here:
//
//   - determinism: pooled-state output must be byte-identical to the
//     expected output computed up front (any state leaking across reuses
//     would break equality);
//   - concurrency: many goroutines hammering Writer and the one-shot
//     reference concurrently must never cross-contaminate each other
//     (run with -race in CI);
//   - aliasing: returned slices must not point into pooled scratch that
//     later compressions overwrite.

// TestPoolConcurrentNoCrossContamination runs GOMAXPROCS×4 goroutines,
// each compressing its own deterministic corpus at varying levels, and
// verifies every output byte-for-byte against a serially precomputed
// expectation plus a decompression round-trip.
func TestPoolConcurrentNoCrossContamination(t *testing.T) {
	type job struct {
		data  []byte
		level int
		want  []byte
	}

	// Precompute expectations serially (also exercises the pools, but
	// sequential use is already proven correct by the C cross-checks).
	const jobsPerWorker = 30
	workers := runtime.GOMAXPROCS(0) * 4
	jobs := make([][]job, workers)
	for w := 0; w < workers; w++ {
		rng := rand.New(rand.NewSource(int64(w) + 1))
		jobs[w] = make([]job, jobsPerWorker)
		for i := range jobs[w] {
			size := 1 + rng.Intn(200_000)
			data := make([]byte, size)
			switch i % 3 {
			case 0: // compressible JSON-ish
				pattern := []byte(fmt.Sprintf(`{"worker":%d,"iter":%d,"pad":"0123456789abcdef"},`, w, i))
				for p := 0; p < size; p += len(pattern) {
					copy(data[p:], pattern)
				}
			case 1: // incompressible
				rng.Read(data)
			default: // highly repetitive
				for p := range data {
					data[p] = byte(w)
				}
			}
			level := []int{-1, 0, 1, 6, 9}[rng.Intn(5)]
			var buf bytes.Buffer
			zw, err := NewWriterLevel(&buf, level)
			if err != nil {
				t.Fatal(err)
			}
			zw.Mtime = uint32(w*1000 + i)
			if _, err := zw.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			jobs[w][i] = job{data: data, level: level, want: buf.Bytes()}
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i, j := range jobs[w] {
				var buf bytes.Buffer
				zw, err := NewWriterLevel(&buf, j.level)
				if err != nil {
					errs <- err
					return
				}
				zw.Mtime = uint32(w*1000 + i)
				if _, err := zw.Write(j.data); err != nil {
					errs <- err
					return
				}
				if err := zw.Close(); err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(buf.Bytes(), j.want) {
					errs <- fmt.Errorf("worker %d job %d: concurrent output differs from precomputed (pool cross-contamination?)", w, i)
					return
				}
				// Round-trip through the stdlib reader as an independent check.
				r, err := NewReader(bytes.NewReader(buf.Bytes()))
				if err != nil {
					errs <- err
					return
				}
				got, err := io.ReadAll(r)
				if err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(got, j.data) {
					errs <- fmt.Errorf("worker %d job %d: round-trip mismatch", w, i)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestPoolResultNotAliased ensures one-shot results do not alias pooled
// scratch buffers: a captured result must stay intact while subsequent
// compressions reuse (and overwrite) the pool.
func TestPoolResultNotAliased(t *testing.T) {
	data := bytes.Repeat([]byte(`{"aliasing":"check"},`), 3000)
	out := zdeflate.GzipCompressLevel(data, 42, 6, 3)
	snapshot := append([]byte(nil), out...)

	other := bytes.Repeat([]byte{0xEE, 0x11}, 100_000)
	for i := 0; i < 20; i++ {
		zdeflate.GzipCompressLevel(other, uint32(i), 1+i%9, 3)
		zdeflate.CompressLevel(other, 1+i%9)
	}
	if !bytes.Equal(out, snapshot) {
		t.Fatal("result slice was overwritten by later pooled compressions (scratch aliasing)")
	}
}

// TestPoolWriterReuseAfterReset re-uses one Writer many times via Reset and
// checks each round against a fresh Writer's output.
func TestPoolWriterReuseAfterReset(t *testing.T) {
	reused := NewWriter(io.Discard)
	for i := 0; i < 50; i++ {
		data := bytes.Repeat([]byte{byte(i), byte(i >> 1), 'x'}, 1000+i*37)

		var fresh, reusedBuf bytes.Buffer
		fw := NewWriter(&fresh)
		fw.Mtime = uint32(i)
		fw.Write(data)
		fw.Close()

		reused.Reset(&reusedBuf)
		reused.Mtime = uint32(i)
		reused.Write(data)
		reused.Close()

		if !bytes.Equal(fresh.Bytes(), reusedBuf.Bytes()) {
			t.Fatalf("round %d: reused Writer output differs from fresh Writer", i)
		}
	}
}
