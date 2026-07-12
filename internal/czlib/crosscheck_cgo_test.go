//go:build cgo
// +build cgo

package czlib

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"testing"

	"github.com/hkloudou/gzip/internal/zdeflate"
)

// The corpus covers inputs that exercise different code paths:
//   - small / empty inputs
//   - highly repetitive data (long matches, RLE-like)
//   - incompressible random data (stored-block decision)
//   - inputs that slide across the 32K window (fill_window slide + slide_hash)
//   - blocks with more than 16383 symbols (symbol buffer flush, multiple blocks)
//   - text/JSON-like data
func crossCheckCorpus() map[string][]byte {
	rng := rand.New(rand.NewSource(42))
	corpus := map[string][]byte{
		"empty":     {},
		"one":       []byte("a"),
		"two":       []byte("{}"),
		"three":     []byte("abc"),
		"golden198": []byte(`{"access_token":"eyJhbGciOiJIUzUxMiJ9.eyJsb2dpbl91c2VyX2tleSI6IjA4N2M2N2E1MGVkNjQwOWY5MzZjMzU3OTdiOTU3ZmFjIn0.4HTb_NXUmYMNf6sJhJbPzZdUtEvV-g0IcKM_OaJl74XaFofsq9_W1MPvPjoxz-Fd_x_WEsotPz7MjUqf_5Uwng"}`),
	}

	corpus["zeros1k"] = make([]byte, 1024)
	corpus["zeros100k"] = make([]byte, 100*1024)

	// Repeating pattern, spans the window
	corpus["repeat200k"] = bytes.Repeat([]byte(`{"key":"value","number":12345,"nested":{"a":"b"}},`), 4096)

	// Pure random (incompressible)
	rand1k := make([]byte, 1024)
	rng.Read(rand1k)
	corpus["random1k"] = rand1k
	rand64k := make([]byte, 64*1024)
	rng.Read(rand64k)
	corpus["random64k"] = rand64k
	rand300k := make([]byte, 300*1024)
	rng.Read(rand300k)
	corpus["random300k"] = rand300k

	// Random over a small alphabet (semi-compressible, many short matches, exercises lazy-match branches)
	semi := make([]byte, 1<<20)
	for i := range semi {
		semi[i] = "abcdefgh"[rng.Intn(8)]
	}
	corpus["semi1m"] = semi

	// Large text-like input (multiple window slides + multiple dynamic blocks)
	var big bytes.Buffer
	for i := 0; big.Len() < 5<<20; i++ {
		fmt.Fprintf(&big, `{"id":%d,"name":"user_%d","email":"user%d@example.com","active":%v},`, i, i*7, i, i%3 == 0)
	}
	corpus["json5m"] = big.Bytes()

	// Symbol-count boundaries: 16383/16384/16385 literals (block flush boundary)
	for _, n := range []int{16382, 16383, 16384, 16385} {
		b := make([]byte, n)
		rng.Read(b) // random → all literal symbols
		corpus[fmt.Sprintf("litboundary%d", n)] = b
	}

	// Window boundaries: 32K / 64K ± 1
	for _, n := range []int{32767, 32768, 32769, 65535, 65536, 65537, 262144} {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.Intn(16)) // semi-compressible
		}
		corpus[fmt.Sprintf("winboundary%d", n)] = b
	}

	// Pathological: long run of 'a' with occasional different characters
	aaa := bytes.Repeat([]byte("a"), 300000)
	for i := 1000; i < len(aaa); i += 7919 {
		aaa[i] = 'b'
	}
	corpus["pathological_a"] = aaa

	return corpus
}

// TestCrossCheckRawAllLevels cross-checks raw deflate output byte-by-byte
// between C zlib and the pure Go implementation over the whole corpus at all
// levels 0-9.
func TestCrossCheckRawAllLevels(t *testing.T) {
	corpus := crossCheckCorpus()
	for name, data := range corpus {
		for level := 0; level <= 9; level++ {
			cOut, err := cgoDeflateRaw(data, level)
			if err != nil {
				t.Fatalf("%s level=%d: C deflate failed: %v", name, level, err)
			}
			goOut := zdeflate.CompressLevel(data, level)
			if !bytes.Equal(cOut, goOut) {
				idx := firstDiff(cOut, goOut)
				t.Errorf("%s level=%d: mismatch (C=%d bytes, Go=%d bytes, first diff@%d)",
					name, level, len(cOut), len(goOut), idx)
			}
		}
	}
}

// TestCrossCheckGzip cross-checks the complete GZIP output (header and trailer included).
func TestCrossCheckGzip(t *testing.T) {
	corpus := crossCheckCorpus()
	timestamps := []int64{0, 1751038273, 4294967295}
	for name, data := range corpus {
		if len(data) == 0 {
			continue
		}
		for _, ts := range timestamps {
			cOut, err := CompressOpts(data, uint32(ts), -1, 3)
			if err != nil {
				t.Fatalf("%s ts=%d: C compression failed: %v", name, ts, err)
			}
			goOut := zdeflate.GzipCompressLevel(data, uint32(ts), -1, 3)
			if !bytes.Equal(cOut, goOut) {
				idx := firstDiff(cOut, goOut)
				t.Errorf("%s ts=%d: GZIP output mismatch (C=%d bytes, Go=%d bytes, first diff@%d)",
					name, ts, len(cOut), len(goOut), idx)
			}
		}
	}
}

// TestCrossCheckCompressLevel cross-checks the level-aware GZIP API, CGO vs pure Go.
func TestCrossCheckCompressLevel(t *testing.T) {
	corpus := crossCheckCorpus()
	for name, data := range corpus {
		if len(data) == 0 || len(data) > 1<<20 {
			continue
		}
		for level := -1; level <= 9; level++ {
			cOut, err := CompressOpts(data, 1751038273, level, 3)
			if err != nil {
				t.Fatalf("%s level=%d: CGO failed: %v", name, level, err)
			}
			goOut := zdeflate.GzipCompressLevel(data, 1751038273, level, 3)
			if !bytes.Equal(cOut, goOut) {
				t.Errorf("%s level=%d: CompressLevel output mismatch", name, level)
			}
		}
	}
}

// TestCrossCheckFuzz cross-checks a large number of randomly generated inputs
// using a fixed seed. Set the ZLIB_FUZZ_ITER / ZLIB_FUZZ_SEED environment
// variables to increase intensity.
func TestCrossCheckFuzz(t *testing.T) {
	iterations := 300
	if testing.Short() {
		iterations = 50
	}
	seed := int64(20260705)
	if v := os.Getenv("ZLIB_FUZZ_ITER"); v != "" {
		fmt.Sscanf(v, "%d", &iterations)
	}
	if v := os.Getenv("ZLIB_FUZZ_SEED"); v != "" {
		fmt.Sscanf(v, "%d", &seed)
	}
	rng := rand.New(rand.NewSource(seed))
	maxSize := 200 * 1024
	if v := os.Getenv("ZLIB_FUZZ_MAXSIZE"); v != "" {
		fmt.Sscanf(v, "%d", &maxSize)
	}
	alphabets := []int{2, 4, 16, 64, 256}
	for i := 0; i < iterations; i++ {
		size := rng.Intn(maxSize)
		alpha := alphabets[rng.Intn(len(alphabets))]
		data := make([]byte, size)
		for j := range data {
			data[j] = byte(rng.Intn(alpha))
		}
		level := rng.Intn(10)

		cOut, err := cgoDeflateRaw(data, level)
		if err != nil {
			t.Fatalf("fuzz#%d: C deflate failed: %v", i, err)
		}
		goOut := zdeflate.CompressLevel(data, level)
		if !bytes.Equal(cOut, goOut) {
			idx := firstDiff(cOut, goOut)
			t.Fatalf("fuzz#%d: mismatch (size=%d alpha=%d level=%d, C=%d bytes, Go=%d bytes, first diff@%d)",
				i, size, alpha, level, len(cOut), len(goOut), idx)
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
