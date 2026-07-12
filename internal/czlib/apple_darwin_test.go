//go:build darwin && cgo
// +build darwin,cgo

package czlib

import (
	"bytes"
	"compress/flate"
	"io"
	"math/rand"
	"testing"

	"github.com/hkloudou/gzip/internal/zdeflate"
)

func appleProbeCorpus() map[string][]byte {
	rng := rand.New(rand.NewSource(42))
	corpus := map[string][]byte{
		"two":       []byte("{}"),
		"golden198": []byte(`{"access_token":"eyJhbGciOiJIUzUxMiJ9.eyJsb2dpbl91c2VyX2tleSI6IjA4N2M2N2E1MGVkNjQwOWY5MzZjMzU3OTdiOTU3ZmFjIn0.4HTb_NXUmYMNf6sJhJbPzZdUtEvV-g0IcKM_OaJl74XaFofsq9_W1MPvPjoxz-Fd_x_WEsotPz7MjUqf_5Uwng"}`),
		"json64k":   bytes.Repeat([]byte(`{"id":1,"name":"user","email":"u@example.com","tags":["a","b"]},`), 1024),
	}
	semi := make([]byte, 300*1024)
	for i := range semi {
		semi[i] = "abcdefghijklmnop"[rng.Intn(16)]
	}
	corpus["semi300k"] = semi
	rnd := make([]byte, 64*1024)
	rng.Read(rnd)
	corpus["random64k"] = rnd
	return corpus
}

// TestAppleSystemLibzByteIdentical verifies empirically that deflate from the
// iOS/macOS system libz is byte-identical to standard zlib (this library).
// It calls the system /usr/lib/libz.1.dylib directly via dlopen, fully
// isolated from the embedded zlib.
func TestAppleSystemLibzByteIdentical(t *testing.T) {
	t.Logf("system libz version: %q", appleSystemLibzVersion())
	corpus := appleProbeCorpus()
	for name, data := range corpus {
		for level := 0; level <= 9; level++ {
			sysOut, err := appleSystemLibzDeflateRaw(data, level)
			if err != nil {
				t.Fatalf("%s level=%d: system libz failed: %v", name, level, err)
			}
			ourOut := zdeflate.CompressLevel(data, level)
			if !bytes.Equal(sysOut, ourOut) {
				t.Errorf("%s level=%d: Apple system libz differs from standard zlib! (sys=%d our=%d first diff@%d)",
					name, level, len(sysOut), len(ourOut), firstDiff(sysOut, ourOut))
			}
		}
	}
	t.Log("Conclusion: Apple system libz deflate output is byte-identical to standard zlib at all levels")
}

// TestAppleCompressionFramework checks empirically whether the output of the
// Apple Compression framework (COMPRESSION_ZLIB, documented as equivalent to
// a level 5 configuration) matches standard zlib at any level. Informational
// test: it only requires the output to be valid raw deflate; the match
// result is printed in the log.
func TestAppleCompressionFramework(t *testing.T) {
	corpus := appleProbeCorpus()
	for name, data := range corpus {
		cfOut, err := appleCompressionZlib(data)
		if err != nil {
			t.Fatalf("%s: Compression framework failed: %v", name, err)
		}

		// Must be valid raw deflate
		r := flate.NewReader(bytes.NewReader(cfOut))
		dec, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("%s: Compression framework output failed to decompress: %v", name, err)
		}
		if !bytes.Equal(dec, data) {
			t.Fatalf("%s: Compression framework output decompressed to mismatched data", name)
		}

		// Byte-by-byte comparison against standard zlib at each level
		matched := -1
		zlib5size := 0
		for level := 1; level <= 9; level++ {
			ours := zdeflate.CompressLevel(data, level)
			if level == 5 {
				zlib5size = len(ours)
			}
			if bytes.Equal(cfOut, ours) {
				matched = level
			}
		}
		if matched >= 0 {
			t.Logf("%s: Compression framework output is byte-identical to zlib level %d (size=%d)",
				name, matched, len(cfOut))
		} else {
			t.Logf("%s: Compression framework output matches no zlib level (1-9) — Apple's own encoder (CF=%d bytes, zlib L5=%d bytes)",
				name, len(cfOut), zlib5size)
		}
	}
}
