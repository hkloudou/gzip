package gzip

// Pure Go vs standard library micro-benchmarks (for pprof-driven work).
// The authoritative speed table — including the real-zlib C++ native leg —
// is produced by `make bench-table` (cmd/crossnative).
//
//	go test -bench 'BenchmarkGzip' -benchmem -run '^$' .

import (
	"bytes"
	stdgzip "compress/gzip"
	"fmt"
	"io"
	"math/rand"
	"testing"

	"github.com/hkloudou/gzip/internal/zdeflate"
)

type benchCase struct {
	name string
	data []byte
}

func benchCases() []benchCase {
	rng := rand.New(rand.NewSource(1))
	json64k := bytes.Repeat([]byte(`{"key":"value","number":12345,"nested":{"a":"b"}},`), 1311)[:64*1024]
	var json1m bytes.Buffer
	for i := 0; json1m.Len() < 1<<20; i++ {
		fmt.Fprintf(&json1m, `{"id":%d,"name":"user_%d","email":"user%d@example.com"},`, i, i*7, i)
	}
	rand1m := make([]byte, 1<<20)
	rng.Read(rand1m)

	return []benchCase{
		{"Small_2B", []byte("{}")},
		{"Medium_198B", []byte(`{"access_token":"eyJhbGciOiJIUzUxMiJ9.eyJsb2dpbl91c2VyX2tleSI6IjA4N2M2N2E1MGVkNjQwOWY5MzZjMzU3OTdiOTU3ZmFjIn0.4HTb_NXUmYMNf6sJhJbPzZdUtEvV-g0IcKM_OaJl74XaFofsq9_W1MPvPjoxz-Fd_x_WEsotPz7MjUqf_5Uwng"}`)},
		{"Large_2KB", bytes.Repeat([]byte(`{"key":"value","number":12345,"nested":{"a":"b"}},`), 40)},
		{"JSON_64KB", json64k},
		{"JSON_1MB", json1m.Bytes()[:1<<20]},
		{"Random_1MB", rand1m},
	}
}

// BenchmarkGzip compares this library's pure Go one-shot with the standard
// library (Writer reused via Reset into a reused buffer). Std Go's output
// bytes differ by design; this is a speed comparison only.
func BenchmarkGzip(b *testing.B) {
	for _, c := range benchCases() {
		b.Run("PureGo/"+c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				zdeflate.GzipCompressLevel(c.data, 1751038273, -1, 3)
			}
		})
		b.Run("StdGo/"+c.name, func(b *testing.B) {
			// Same job as the PureGo leg: reuse the compressor, deliver a
			// fresh exact-size result slice per op (see cmd/crossnative).
			var buf bytes.Buffer
			w := stdgzip.NewWriter(&buf)
			var sink []byte
			b.SetBytes(int64(len(c.data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buf.Reset()
				w.Reset(&buf)
				if _, err := w.Write(c.data); err != nil {
					b.Fatal(err)
				}
				if err := w.Close(); err != nil {
					b.Fatal(err)
				}
				out := make([]byte, buf.Len())
				copy(out, buf.Bytes())
				sink = out
			}
			_ = sink
		})
	}
}

// BenchmarkGzipParallel measures the pooled pure Go path under concurrency.
func BenchmarkGzipParallel(b *testing.B) {
	data := benchCases()[3].data // JSON_64KB
	b.Run("PureGo/JSON_64KB", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				zdeflate.GzipCompressLevel(data, 1751038273, -1, 3)
			}
		})
	})
}

// BenchmarkGzipLevels measures pure Go performance at each compression level.
func BenchmarkGzipLevels(b *testing.B) {
	data := benchCases()[4].data // JSON_1MB
	for _, level := range []int{0, 1, 6, 9} {
		b.Run(fmt.Sprintf("PureGo/level%d", level), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				zdeflate.GzipCompressLevel(data, 0, level, 3)
			}
		})
	}
}

// BenchmarkWriterStream measures the streaming Writer path (Write+Flush per
// chunk, as an HTTP/SSE compressor would run) with pooled state reuse.
func BenchmarkWriterStream(b *testing.B) {
	chunk := bytes.Repeat([]byte(`{"event":"tick","seq":123456},`), 32) // ~1KB
	b.SetBytes(int64(len(chunk) * 16))
	b.ReportAllocs()
	w := NewWriter(io.Discard)
	for i := 0; i < b.N; i++ {
		w.Reset(io.Discard)
		for j := 0; j < 16; j++ {
			if _, err := w.Write(chunk); err != nil {
				b.Fatal(err)
			}
			if err := w.Flush(); err != nil {
				b.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
