package gzip

// The C reference for tests in this package is the gzip_ref referee run as a
// subprocess — built from the OFFICIAL zlib 1.3.1 sources by `make
// native-build` (see native/gzip_ref.cpp). No C code lives in this
// repository, and there is no cgo: when the referee binary is absent the
// C-comparison legs skip and only the pure Go legs run. CI always builds the
// referee on ubuntu/macos, so skips cannot hide a regression there.

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// refPath returns the referee binary path, or "" when not built.
func refPath() string {
	if p := os.Getenv("ZLIB_REF"); p != "" {
		return p
	}
	for _, p := range []string{"bin/gzip_ref", "bin/gzip_ref.exe"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// needRef skips the calling test when the referee is absent.
func needRef(t *testing.T) string {
	t.Helper()
	p := refPath()
	if p == "" {
		t.Skip("C reference leg skipped: bin/gzip_ref not built (run `make native-build`)")
	}
	return p
}

func refRun(t *testing.T, bin string, input []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdin = bytes.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v (stderr: %s)", bin, args, err, stderr.String())
	}
	return out.Bytes()
}

// refGzip has the referee produce a framed GZIP stream (raw deflate with the
// same 10+8 byte framing as gzip.Writer, XFL=0). flushAt >= 0 inserts a
// Z_SYNC_FLUSH after the first flushAt input bytes.
func refGzip(t *testing.T, bin string, input []byte, level int, mtime uint32, osByte byte, flushAt int) []byte {
	t.Helper()
	args := []string{"compress", fmt.Sprintf("%d", level), fmt.Sprintf("%d", mtime), fmt.Sprintf("%d", osByte)}
	if flushAt >= 0 {
		args = append(args, fmt.Sprintf("%d", flushAt))
	}
	return refRun(t, bin, input, args...)
}

// refStream drives the referee's raw deflate with an explicit (chunk, flush)
// call sequence — the C twin of zdeflate.Deflater.Deflate calls.
func refStream(t *testing.T, bin string, input []byte, level int, chunks []int, flushes []int) []byte {
	t.Helper()
	if len(chunks) != len(flushes) {
		t.Fatal("refStream: chunks/flushes length mismatch")
	}
	args := make([]string, 0, 2+len(chunks))
	args = append(args, "stream", fmt.Sprintf("%d", level))
	for i := range chunks {
		args = append(args, fmt.Sprintf("%d@%d", flushes[i], chunks[i]))
	}
	return refRun(t, bin, input, args...)
}

// refDeflateRaw is the one-shot raw deflate counterpart of
// zdeflate.CompressLevel: a single deflate(Z_FINISH) call.
func refDeflateRaw(t *testing.T, bin string, input []byte, level int) []byte {
	t.Helper()
	return refStream(t, bin, input, level, []int{len(input)}, []int{4})
}

// refHeader has zlib itself (deflateSetHeader) produce the full GZIP stream.
// Empty name/comment mean "absent" (gz_header NULL); nil extra is absent
// while an empty non-nil extra is present with xlen=0 — the same semantics
// as gzip.Writer's Header fields.
func refHeader(t *testing.T, bin string, input []byte, level int, mtime uint32, osByte byte, extra []byte, name, comment string) []byte {
	t.Helper()
	strArg := func(s string) string {
		if s == "" {
			return "-"
		}
		b, err := latin1Bytes(s)
		if err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(b)
	}
	extraArg := "-"
	if extra != nil {
		extraArg = hex.EncodeToString(extra)
	}
	return refRun(t, bin, input, "header",
		fmt.Sprintf("%d", level), fmt.Sprintf("%d", mtime), fmt.Sprintf("%d", osByte),
		strArg(name), strArg(comment), extraArg)
}

// latin1Bytes converts a Go string to Latin-1, one byte per rune.
func latin1Bytes(s string) ([]byte, error) {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		if r == 0 || r > 0xff {
			return nil, fmt.Errorf("non-Latin-1 rune %q", r)
		}
		b = append(b, byte(r))
	}
	return b, nil
}
