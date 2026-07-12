package czlib

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"

	"github.com/hkloudou/gzip/internal/zdeflate"
)

// ============================================================
// Benchmark: C reference (cgo) vs this library's pure Go port
// ============================================================
//
// Run both side by side (requires CGO):
//   CGO_ENABLED=1 go test -bench 'BenchmarkGzip' -benchmem ./internal/czlib/
//
// Pure Go only (works on any platform / cross-compilation):
//   CGO_ENABLED=0 go test -bench 'BenchmarkGzip/PureGo' -benchmem ./internal/czlib/
//

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

// BenchmarkGzip compares CGO (C zlib) and the pure Go port within the same binary.
func BenchmarkGzip(b *testing.B) {
	for _, c := range benchCases() {
		b.Run("CGO/"+c.name, func(b *testing.B) {
			if !HasCGO() {
				b.Skip("requires CGO_ENABLED=1")
			}
			b.SetBytes(int64(len(c.data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := CompressOpts(c.data, 1751038273, -1, 3); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("PureGo/"+c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				zdeflate.GzipCompressLevel(c.data, 1751038273, -1, 3)
			}
		})
	}
}

// BenchmarkGzipParallel compares the two under concurrency.
func BenchmarkGzipParallel(b *testing.B) {
	data := benchCases()[3].data // JSON_64KB
	b.Run("CGO/JSON_64KB", func(b *testing.B) {
		if !HasCGO() {
			b.Skip("requires CGO_ENABLED=1")
		}
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if _, err := CompressOpts(data, 1751038273, -1, 3); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
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

// BenchmarkGzipLevels measures performance at each compression level.
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
		b.Run(fmt.Sprintf("CGO/level%d", level), func(b *testing.B) {
			if !HasCGO() {
				b.Skip("requires CGO_ENABLED=1")
			}
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := CompressOpts(data, 0, level, 3); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
