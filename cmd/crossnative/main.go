// crossnative is the cross-check / benchmark orchestrator. The referees are
// C++ native binaries (native/gzip_ref.cpp — real zlib as a subprocess, no
// C code from this repository), normally two builds:
//
//	official  built from the downloaded official zlib 1.3.1 tarball
//	          (pinned SHA-256) — the deterministic correct answer
//	system    linked against the system zlib
//
// -mode check: full matrix of corpus × level × flush × header fields; every
// referee's output and the pure Go library's output (internal/zdeflate +
// gzip.Writer) must match byte-for-byte. Any mismatch exits non-zero and
// prints hex context around the first differing byte.
//
// -mode bench: measures throughput of C++ native / pure Go / std Go on the
// same corpus (native is timed by an in-process loop in the subprocess, the
// Go sides use testing.Benchmark) and renders a markdown table;
// -update-readme writes the table into the README's AUTOBENCH marker block.
//
// This tool is pure Go and needs no cgo; it only shells out to the referee.
package main

import (
	"bytes"
	stdgzip "compress/gzip"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	gzip "github.com/hkloudou/gzip"
	"github.com/hkloudou/gzip/internal/zdeflate"
)

const (
	ts     = uint32(1751038273)
	osByte = byte(3)

	markerBegin = "<!-- AUTOBENCH:BEGIN -->"
	markerEnd   = "<!-- AUTOBENCH:END -->"

	locBegin = "<!-- AUTOLOC:BEGIN -->"
	locEnd   = "<!-- AUTOLOC:END -->"
)

type corpusEntry struct {
	name string
	data []byte
}

// corpus is generated fully deterministically (random data uses a fixed
// seed) so all three sides are fed exactly the same bytes.
func corpus() []corpusEntry {
	rng := rand.New(rand.NewSource(1))

	var json1m bytes.Buffer
	for i := 0; json1m.Len() < 1<<20; i++ {
		fmt.Fprintf(&json1m, `{"id":%d,"name":"user_%d","email":"user%d@example.com"},`, i, i*7, i)
	}
	rand256k := make([]byte, 256<<10)
	rng.Read(rand256k)

	binary := make([]byte, 100_000)
	for i := range binary {
		binary[i] = byte(i * 31)
	}

	return []corpusEntry{
		{"1B", []byte("a")},
		{"2B_json", []byte("{}")},
		{"198B_token", []byte(`{"access_token":"eyJhbGciOiJIUzUxMiJ9.eyJsb2dpbl91c2VyX2tleSI6IjA4N2M2N2E1MGVkNjQwOWY5MzZjMzU3OTdiOTU3ZmFjIn0.4HTb_NXUmYMNf6sJhJbPzZdUtEvV-g0IcKM_OaJl74XaFofsq9_W1MPvPjoxz-Fd_x_WEsotPz7MjUqf_5Uwng"}`)},
		{"2KB_json", bytes.Repeat([]byte(`{"key":"value","number":12345,"nested":{"a":"b"}},`), 40)},
		{"100KB_repeat", bytes.Repeat([]byte("a"), 100_000)},
		{"100KB_binary", binary},
		{"256KB_random", rand256k},
		{"1MB_json", json1m.Bytes()[:1<<20]},
	}
}

func main() {
	mode := flag.String("mode", "check", "check | bench | patch | loc")
	native := flag.String("native", "./bin/gzip_ref", "comma-separated paths of native reference binaries")
	benchTime := flag.Duration("benchtime", time.Second, "target duration per benchmark case")
	updateReadme := flag.String("update-readme", "", "write the bench table into this README's AUTOBENCH block")
	out := flag.String("out", "", "also save the bench markdown to a file")
	from := flag.String("from", "", "patch mode: read a previously generated bench markdown file")
	flag.Parse()

	bins := strings.Split(*native, ",")
	switch *mode {
	case "check":
		for _, bin := range bins {
			runCheck(strings.TrimSpace(bin))
		}
	case "bench":
		runBench(strings.TrimSpace(bins[0]), *benchTime, *updateReadme, *out)
	case "loc":
		// Count Go lines (product / tests / test infrastructure) and render
		// a markdown table; -update-readme writes the AUTOLOC block.
		// Pure file operation, no CGO needed.
		md := renderLoc()
		fmt.Print(md)
		if *out != "" {
			must(os.WriteFile(*out, []byte(md), 0o644))
		}
		if *updateReadme != "" {
			must(patchSection(*updateReadme, locBegin, locEnd, md))
			fmt.Fprintf(os.Stderr, "updated AUTOLOC block in %s\n", *updateReadme)
		}
	case "patch":
		// Pure file operation: patch a table previously saved with -out into
		// the README (used by CI retry commits, avoiding a benchmark re-run
		// on every retry)
		if *from == "" || *updateReadme == "" {
			fmt.Fprintln(os.Stderr, "crossnative: patch mode requires -from and -update-readme")
			os.Exit(2)
		}
		table, err := os.ReadFile(*from)
		must(err)
		must(patchSection(*updateReadme, markerBegin, markerEnd, string(table)))
		fmt.Fprintf(os.Stderr, "updated AUTOBENCH block in %s (from %s)\n", *updateReadme, *from)
	default:
		fmt.Fprintf(os.Stderr, "crossnative: unknown mode %q\n", *mode)
		os.Exit(2)
	}
}

/* ------------------------------------------------------------------ */
/* native subprocess                                                    */
/* ------------------------------------------------------------------ */

func runNative(bin string, stdin []byte, args ...string) []byte {
	cmd := exec.Command(bin, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "crossnative: %s %v failed: %v\n%s", bin, args, err, errBuf.String())
		os.Exit(1)
	}
	return outBuf.Bytes()
}

func nativeVersion(bin string) string {
	return strings.TrimSpace(string(runNative(bin, nil, "version")))
}

/* ------------------------------------------------------------------ */
/* check                                                                */
/* ------------------------------------------------------------------ */

var failures int

func mismatch(label string, a, b []byte, aName, bName string) {
	off := 0
	n := min(len(a), len(b))
	for off < n && a[off] == b[off] {
		off++
	}
	lo := max(0, off-16)
	fmt.Fprintf(os.Stderr, "MISMATCH %s: %s(%d bytes) vs %s(%d bytes), first difference @%d\n  %s: % x\n  %s: % x\n",
		label, aName, len(a), bName, len(b), off,
		aName, a[lo:min(len(a), off+16)],
		bName, b[lo:min(len(b), off+16)])
	failures++
}

func expectEqual(label string, native, pure []byte) {
	if !bytes.Equal(native, pure) {
		mismatch(label, native, pure, "native", "pureGo")
	}
}

func runCheck(bin string) {
	fmt.Printf("== cross-check: native=%s (real zlib %s) | this library (pure Go) ==\n", bin, nativeVersion(bin))
	entries := corpus()
	levels := []int{-1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	checks := 0

	// 1. One-shot compression: corpus × level
	for _, e := range entries {
		for _, level := range levels {
			nat := runNative(bin, e.data, "compress",
				strconv.Itoa(level), strconv.FormatUint(uint64(ts), 10), strconv.Itoa(int(osByte)))
			pure := zdeflate.GzipCompressLevel(e.data, ts, level, osByte)
			expectEqual(fmt.Sprintf("compress/%s/level=%d", e.name, level), nat, pure)
			checks++
		}
	}
	fmt.Printf("  one-shot compression: %d cases\n", checks)

	// 1b. MTIME × OS byte dimensions: both only affect header bytes, but
	// must still match byte-for-byte
	prev := checks
	dimCorpus := []corpusEntry{entries[2], entries[5], entries[7]} // 198B token / 100KB binary / 1MB json
	for _, e := range dimCorpus {
		for _, level := range []int{-1, 0, 1, 6, 9} {
			for _, hTS := range []uint32{0, ts, 0xFFFFFFFF} {
				for _, hOS := range []byte{0, 3, 11, 255} {
					nat := runNative(bin, e.data, "compress", strconv.Itoa(level),
						strconv.FormatUint(uint64(hTS), 10), strconv.Itoa(int(hOS)))
					pure := zdeflate.GzipCompressLevel(e.data, hTS, level, hOS)
					expectEqual(fmt.Sprintf("tsos/%s/level=%d/ts=%d/os=%d", e.name, level, hTS, hOS),
						nat, pure)
					checks++
				}
			}
		}
	}
	fmt.Printf("  MTIME × OS dimensions: %d cases\n", checks-prev)

	// 2. Z_SYNC_FLUSH: corpus × level × flush position
	prev = checks
	for _, e := range entries {
		if len(e.data) < 2 {
			continue
		}
		for _, level := range levels {
			for _, at := range flushPoints(len(e.data)) {
				nat := runNative(bin, e.data, "compress",
					strconv.Itoa(level), strconv.FormatUint(uint64(ts), 10),
					strconv.Itoa(int(osByte)), strconv.Itoa(at))
				pure := pureSyncFlush(e.data, ts, level, osByte, at)
				expectEqual(fmt.Sprintf("syncflush/%s/level=%d/at=%d", e.name, level, at), nat, pure)
				checks++
			}
		}
	}
	fmt.Printf("  Z_SYNC_FLUSH: %d cases\n", checks-prev)

	// 2b. Streaming call sequences: raw deflate driven by identical
	// (chunk, flush) sequences on both sides, covering every flush type
	prev = checks
	seqData := entries[7].data[:512*1024] // 1MB json prefix
	seqs := []struct {
		name    string
		chunks  []int
		flushes []int
	}{
		{"sync_thirds", []int{170 << 10, 170 << 10, 512<<10 - 2*(170<<10)}, []int{2, 2, 4}},
		{"partial_full_mix", []int{100 << 10, 200 << 10, 212 << 10}, []int{1, 3, 4}},
		{"writer_sequence", []int{256 << 10, 0, 256 << 10, 0}, []int{0, 2, 0, 4}},
		{"empty_flushes", []int{0, 0, 512 << 10}, []int{2, 3, 4}},
	}
	for _, level := range []int{0, 1, 6, 9} {
		for _, sq := range seqs {
			args := []string{"stream", strconv.Itoa(level)}
			for i := range sq.chunks {
				args = append(args, fmt.Sprintf("%d@%d", sq.flushes[i], sq.chunks[i]))
			}
			nat := runNative(bin, seqData, args...)
			pure := pureStream(seqData, level, sq.chunks, sq.flushes)
			expectEqual(fmt.Sprintf("stream/%s/level=%d", sq.name, level), nat, pure)
			checks++
		}
	}
	fmt.Printf("  streaming call sequences: %d cases\n", checks-prev)

	// 3. Full header-field parameter matrix: Name/Comment/Extra combinations
	// × OS byte × MTIME × level × several content shapes. The native side
	// goes through real zlib's deflateSetHeader; gzip.Writer, after its
	// XFL byte is fixed up, must match byte-for-byte.
	prev = checks
	headerCases := []struct {
		label   string
		name    string
		comment string
		extra   []byte
	}{
		{"none", "", "", nil},
		{"name", "data.json", "", nil},
		{"comment", "", "a comment ×÷", nil},
		{"extra", "", "", []byte{0x01, 0x02, 0x00, 0xff}},
		{"empty-extra", "", "", []byte{}}, // non-nil empty Extra: FEXTRA + xlen=0
		{"all", "naïve.json", "café ÀÿÞ", []byte{0xde, 0xad, 0xbe, 0xef}},
		{"long", strings.Repeat("n", 200) + ".bin", strings.Repeat("c", 300),
			bytes.Repeat([]byte{0x55, 0xaa}, 500)},
	}
	// Content shapes: small JSON token / binary / random incompressible / 1MB JSON
	headerCorpus := []corpusEntry{entries[2], entries[5], entries[6], entries[7]}
	for _, hc := range headerCases {
		for _, e := range headerCorpus {
			for _, hOS := range []byte{3, 255} {
				for _, hTS := range []uint32{0, ts} {
					for _, level := range []int{-1, 0, 1, 2, 6, 9} {
						nat := runNative(bin, e.data, "header",
							strconv.Itoa(level), strconv.FormatUint(uint64(hTS), 10), strconv.Itoa(int(hOS)),
							hexOrDash(latin1(hc.name)), hexOrDash(latin1(hc.comment)), hexOrDash(hc.extra))

						var buf bytes.Buffer
						w, err := gzip.NewWriterLevel(&buf, level)
						must(err)
						w.Mtime = hTS
						w.OS = hOS
						w.Name = hc.name
						w.Comment = hc.comment
						w.Extra = hc.extra
						_, err = w.Write(e.data)
						must(err)
						must(w.Close())
						writerOut := buf.Bytes()
						if writerOut[8] != 0 {
							// First assert the Writer really always writes XFL=0,
							// then fix it up to C's per-level value
							fmt.Fprintf(os.Stderr, "MISMATCH header-xfl/%s/level=%d: Writer XFL should be 0, got %#x\n",
								hc.label, level, writerOut[8])
							failures++
						}
						writerOut[8] = cXFL(level)

						label := fmt.Sprintf("header/%s/%s/os=%d/ts=%d/level=%d",
							hc.label, e.name, hOS, hTS, level)
						if level == 0 {
							// At level 0 the stored-block splitting depends on the
							// call sequence: the Writer does NO_FLUSH+FINISH, while
							// the C reference here does a single FINISH — C zlib
							// itself produces different bytes for the two sequences
							// (decompresses identically, see the note in
							// gzip_test.go); unrelated to header fields. The Writer
							// is checked for a byte-identical header prefix plus a
							// decompression round-trip; same-sequence level-0 parity
							// is covered by the streaming cases above.
							hdrLen := headerPrefixLen(hc.name, hc.comment, hc.extra)
							if !bytes.Equal(writerOut[:hdrLen], nat[:hdrLen]) {
								mismatch(label+"/prefix", writerOut[:hdrLen], nat[:hdrLen], "goWriter", "native")
							}
							verifyDecompress(label, buf.Bytes(), e.data)
						} else {
							expectEqual(label, nat, writerOut)
						}
						checks++
					}
				}
			}
		}
	}
	fmt.Printf("  header full-parameter matrix (deflateSetHeader × Writer): %d cases\n", checks-prev)

	// 4. Empty input: the one-shot API is defined to return nil, so
	// cross-check gzip.Writer (pure Go) against native instead
	prev = checks
	for _, level := range levels {
		nat := runNative(bin, nil, "compress",
			strconv.Itoa(level), strconv.FormatUint(uint64(ts), 10), strconv.Itoa(int(osByte)))
		var buf bytes.Buffer
		w, err := gzip.NewWriterLevel(&buf, level)
		must(err)
		w.Mtime = ts
		must(w.Close())
		if !bytes.Equal(nat, buf.Bytes()) {
			mismatch(fmt.Sprintf("empty/level=%d", level), nat, buf.Bytes(), "native", "goWriter")
		}
		checks++
	}
	fmt.Printf("  empty input (Writer): %d cases\n", checks-prev)

	if failures > 0 {
		fmt.Fprintf(os.Stderr, "crossnative: %d mismatches (out of %d cases)\n", failures, checks)
		os.Exit(1)
	}
	fmt.Printf("PASS: %d cases byte-identical (real zlib vs pure Go)\n\n", checks)
}

// pureStream replays a (chunk, flush) call sequence with the pure Go
// streaming implementation (raw deflate) — the twin of gzip_ref stream mode.
func pureStream(data []byte, level int, chunks, flushes []int) []byte {
	d, err := zdeflate.NewDeflater(level)
	must(err)
	defer d.Close()
	var raw bytes.Buffer
	off := 0
	for i := range chunks {
		n := chunks[i]
		must(d.Deflate(data[off:off+n], flushes[i], &raw))
		off += n
	}
	return raw.Bytes()
}

// pureSyncFlush replays the single-SYNC semantics with this library's
// compression implementation (zdeflate):
// deflate(data[:at], Z_SYNC_FLUSH); deflate(data[at:], Z_FINISH),
// wrapped in a gzip frame — same semantics as the C reference's
// CompressWithSyncFlush.
func pureSyncFlush(data []byte, ts uint32, level int, osByte byte, at int) []byte {
	d, err := zdeflate.NewDeflater(level)
	must(err)
	defer d.Close()
	var raw bytes.Buffer
	must(d.Deflate(data[:at], zdeflate.SyncFlush, &raw))
	must(d.Deflate(data[at:], zdeflate.Finish, &raw))

	out := make([]byte, 0, 18+raw.Len())
	out = append(out, 0x1f, 0x8b, 0x08, 0x00,
		byte(ts), byte(ts>>8), byte(ts>>16), byte(ts>>24), 0x00, osByte)
	out = append(out, raw.Bytes()...)
	crc := crc32.ChecksumIEEE(data)
	n := uint32(len(data))
	return append(out,
		byte(crc), byte(crc>>8), byte(crc>>16), byte(crc>>24),
		byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
}

// flushPoints returns the sync-flush position matrix, including both
// boundaries: 0 = Flush before writing any data (emits an empty stored
// block), n = Flush after all data, then finish.
func flushPoints(n int) []int {
	pts := []int{0, 1, n / 2, n - 1, n}
	var out []int
	for _, p := range pts {
		if p >= 0 && p <= n && (len(out) == 0 || out[len(out)-1] != p) {
			out = append(out, p)
		}
	}
	return out
}

// headerPrefixLen returns the byte length of the GZIP header (including
// optional fields).
func headerPrefixLen(name, comment string, extra []byte) int {
	n := 10
	if extra != nil {
		n += 2 + len(extra)
	}
	if name != "" {
		n += len(latin1(name)) + 1
	}
	if comment != "" {
		n += len(latin1(comment)) + 1
	}
	return n
}

// verifyDecompress checks that a GZIP stream can be decompressed by the
// standard library and restores the payload.
func verifyDecompress(label string, gz, want []byte) {
	r, err := stdgzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		fmt.Fprintf(os.Stderr, "MISMATCH %s: decompress failed: %v\n", label, err)
		failures++
		return
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, want) {
		fmt.Fprintf(os.Stderr, "MISMATCH %s: decompressed output mismatch (%v)\n", label, err)
		failures++
	}
}

// cXFL is the XFL byte C zlib computes per level on the deflateSetHeader path.
func cXFL(level int) byte {
	if level == -1 {
		level = 6
	}
	switch {
	case level == 9:
		return 2
	case level < 2:
		return 4
	default:
		return 0
	}
}

func latin1(s string) []byte {
	if s == "" {
		return nil
	}
	b := make([]byte, 0, len(s))
	for _, v := range s {
		if v == 0 || v > 0xff {
			panic("header strings in the corpus must be Latin-1")
		}
		b = append(b, byte(v))
	}
	return b
}

func hexOrDash(b []byte) string {
	if b == nil {
		return "-"
	}
	return hex.EncodeToString(b)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "crossnative:", err)
		os.Exit(1)
	}
}

/* ------------------------------------------------------------------ */
/* bench                                                                */
/* ------------------------------------------------------------------ */

type benchRow struct {
	name    string
	size    int
	nativeN float64 // ns/op
	pure    testing.BenchmarkResult
	std     testing.BenchmarkResult // standard library compress/gzip (speed context only)
}

func runBench(bin string, benchTime time.Duration, readmePath, outPath string) {
	cases := []corpusEntry{
		{"2 B", []byte("{}")},
		{"198 B JSON token", corpus()[2].data},
		{"2 KB JSON", corpus()[3].data},
		{"64 KB JSON", bytes.Repeat([]byte(`{"key":"value","number":12345,"nested":{"a":"b"}},`), 1311)[:64*1024]},
		{"1 MB JSON", corpus()[7].data},
		{"1 MB random (incompressible)", func() []byte {
			b := make([]byte, 1<<20)
			rand.New(rand.NewSource(1)).Read(b)
			return b
		}()},
	}

	// Register the testing flags and set benchtime (required to use
	// testing.Benchmark from a main package)
	testing.Init()
	if f := flag.Lookup("test.benchtime"); f != nil {
		_ = f.Value.Set(benchTime.String())
	}

	rows := make([]benchRow, 0, len(cases))
	for _, c := range cases {
		row := benchRow{name: c.name, size: len(c.data)}
		row.nativeN = benchNative(bin, c.data, benchTime)
		row.pure = benchGo(c.data, func(data []byte) {
			zdeflate.GzipCompressLevel(data, ts, -1, 3)
		})
		// Standard library compress/gzip: performance context only — its
		// output bytes differ by design (that is why this library exists).
		// The Writer is reused via Reset, mirroring the pooled reuse of the
		// other legs.
		var stdBuf bytes.Buffer
		stdW := stdgzip.NewWriter(&stdBuf)
		row.std = benchGo(c.data, func(data []byte) {
			stdBuf.Reset()
			stdW.Reset(&stdBuf)
			if _, err := stdW.Write(data); err != nil {
				panic(err)
			}
			if err := stdW.Close(); err != nil {
				panic(err)
			}
		})
		fmt.Fprintf(os.Stderr, "bench %-28s native=%s pureGo=%s stdGo=%s\n",
			c.name, fmtNs(row.nativeN),
			fmtNs(float64(row.pure.NsPerOp())), fmtNs(float64(row.std.NsPerOp())))
		rows = append(rows, row)
	}

	md := renderMarkdown(bin, rows)
	fmt.Print(md)
	if outPath != "" {
		must(os.WriteFile(outPath, []byte(md), 0o644))
	}
	if readmePath != "" {
		must(patchSection(readmePath, markerBegin, markerEnd, md))
		fmt.Fprintf(os.Stderr, "updated AUTOBENCH block in %s\n", readmePath)
	}
}

// benchGo measures one compression function with testing.Benchmark
// (including memory stats). testing.Benchmark auto-calibrates the iteration
// count based on test.benchtime (set in runBench).
func benchGo(data []byte, fn func([]byte)) testing.BenchmarkResult {
	return testing.Benchmark(func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			fn(data)
		}
	})
}

// benchNative first calibrates the iteration count (targeting benchTime),
// then takes ns/op from the real run.
func benchNative(bin string, data []byte, benchTime time.Duration) float64 {
	calib := parseBenchJSON(runNative(bin, data, "bench", "6", "10"))
	checkBenchInput(calib, len(data))
	ratio := float64(benchTime.Nanoseconds()) / calib.NsPerOp
	iters := 10
	if ratio > float64(iters) { // NsPerOp is validated >0, ratio cannot be NaN/Inf
		iters = int(ratio)
	}
	if iters > 2_000_000 {
		iters = 2_000_000
	}
	res := parseBenchJSON(runNative(bin, data, "bench", "6", strconv.Itoa(iters)))
	checkBenchInput(res, len(data))
	return res.NsPerOp
}

// checkBenchInput verifies that the native side actually received the full
// stdin input, so a broken pipe cannot make the bench silently measure
// truncated data.
func checkBenchInput(r benchJSON, want int) {
	if r.InBytes != want {
		fmt.Fprintf(os.Stderr, "crossnative: native bench in_bytes=%d, want %d (broken stdin pipe?)\n",
			r.InBytes, want)
		os.Exit(1)
	}
}

type benchJSON struct {
	Iters    int     `json:"iters"`
	NsPerOp  float64 `json:"ns_per_op"`
	InBytes  int     `json:"in_bytes"`
	OutBytes int     `json:"out_bytes"`
}

// parseBenchJSON parses the native bench output and validates its fields —
// this JSON is hand-coupled to the printf in gzip_ref.cpp, so misaligned
// field names/units must fail hard rather than letting zero values silently
// flow into the auto-committed README table.
func parseBenchJSON(b []byte) benchJSON {
	var r benchJSON
	must(json.Unmarshal(bytes.TrimSpace(b), &r))
	if r.Iters <= 0 || r.NsPerOp <= 0 || r.OutBytes <= 0 {
		fmt.Fprintf(os.Stderr, "crossnative: invalid native bench output: %s\n", bytes.TrimSpace(b))
		os.Exit(1)
	}
	return r
}

func fmtNs(ns float64) string {
	switch {
	case ns >= 1e6:
		return fmt.Sprintf("%.1f ms", ns/1e6)
	case ns >= 1e3:
		return fmt.Sprintf("%.1f µs", ns/1e3)
	default:
		return fmt.Sprintf("%.0f ns", ns)
	}
}

func cell(ns float64, size int) string {
	if size >= 2048 {
		mbps := float64(size) / (ns / 1e9) / 1e6
		return fmt.Sprintf("%s (%.0f MB/s)", fmtNs(ns), mbps)
	}
	return fmtNs(ns) + "/op"
}

func renderMarkdown(bin string, rows []benchRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Level 6, each op is a full compression (reset + deflate + CRC + gzip framing); every column reuses compressor state (C++ via deflateReset, the Go columns via sync.Pool / Writer.Reset).\n\n")
	fmt.Fprintf(&b, "- **C++ Native**: real zlib %s (built from the official 1.3.1 sources) looping in-process — the C performance ceiling and the byte-correctness referee\n", nativeVersion(bin))
	b.WriteString("- **Pure Go**: this library\n")
	b.WriteString("- **Std Go**: the standard library compress/gzip — speed context only; its output bytes differ by design, which is the reason this library exists\n\n")
	b.WriteString("**Speed** (ratios are relative speed of Pure Go; higher = Pure Go faster):\n\n")
	b.WriteString("| Input | C++ Native | Pure Go | Std Go | Pure Go / C++ Native | Pure Go / Std Go |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range rows {
		pureNs, stdNs := float64(r.pure.NsPerOp()), float64(r.std.NsPerOp())
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			r.name, cell(r.nativeN, r.size), cell(pureNs, r.size), cell(stdNs, r.size),
			ratioCell(r.nativeN/pureNs), ratioCell(stdNs/pureNs))
	}
	b.WriteString("\n**Memory** (Go heap per op; the native referee is a subprocess and has no Go heap; Std Go compresses into a reused bytes.Buffer while Pure Go returns a fresh exact-size slice per op):\n\n")
	b.WriteString("| Input | Pure Go | Std Go |\n")
	b.WriteString("|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.name, memCell(r.pure), memCell(r.std))
	}
	fmt.Fprintf(&b, "\n*%s · %s · go %s · %s/%s · commit `%s` (auto-updated by CI on push to main)*\n",
		time.Now().UTC().Format("2006-01-02 15:04 UTC"), cpuModel(),
		strings.TrimPrefix(runtime.Version(), "go"), runtime.GOOS, runtime.GOARCH, commitSHA())
	return b.String()
}

// ratioCell renders a relative-speed ratio, bolding clear wins.
func ratioCell(ratio float64) string {
	if ratio >= 1.15 {
		return fmt.Sprintf("**%.2f× faster**", ratio)
	}
	return fmt.Sprintf("%.2f×", ratio)
}

func memCell(r testing.BenchmarkResult) string {
	return fmt.Sprintf("%s · %d allocs", fmtBytes(r.AllocedBytesPerOp()), r.AllocsPerOp())
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func cpuModel() string {
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "model name") {
					if _, v, ok := strings.Cut(line, ":"); ok {
						return strings.TrimSpace(v)
					}
				}
			}
		}
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return runtime.GOARCH
}

func commitSHA() string {
	if sha := os.Getenv("GITHUB_SHA"); len(sha) >= 7 {
		return sha[:7]
	}
	if out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return "unknown"
}

func patchSection(path, begin, end, content string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := string(data)
	b := strings.Index(s, begin)
	e := strings.Index(s, end)
	if b < 0 || e < 0 || e < b {
		return fmt.Errorf("README is missing %s / %s markers", begin, end)
	}
	updated := s[:b+len(begin)] + "\n" + content + s[e:]
	return os.WriteFile(path, []byte(updated), 0o644)
}

/* ------------------------------------------------------------------ */
/* loc: Go line counting                                                */
/* ------------------------------------------------------------------ */

type locBucket struct {
	files int
	lines int
}

// renderLoc counts Go source lines in the repo (physical lines) in three
// buckets: product / tests (*_test.go) / test infrastructure (the non-test
// code of the cross-check orchestrator). The C++ referee tool is not Go code
// and is not counted.
func renderLoc() string {
	var product, tests, infra locBucket
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "bin" || name == ".github" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := bytes.Count(data, []byte("\n"))
		if len(data) > 0 && data[len(data)-1] != '\n' {
			lines++
		}
		switch {
		case strings.HasSuffix(path, "_test.go"):
			tests.files++
			tests.lines += lines
		case strings.HasPrefix(path, "cmd/"):
			infra.files++
			infra.lines += lines
		default: // root package + internal/zdeflate
			product.files++
			product.lines += lines
		}
		return nil
	})
	must(err)

	var b strings.Builder
	b.WriteString("| Category | Files | Go lines |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| Product (root package + internal/zdeflate, pure Go) | %d | %d |\n", product.files, product.lines)
	fmt.Fprintf(&b, "| Tests (*_test.go) | %d | %d |\n", tests.files, tests.lines)
	fmt.Fprintf(&b, "| Test infrastructure (cmd/crossnative, non-test) | %d | %d |\n", infra.files, infra.lines)
	fmt.Fprintf(&b, "\n*(tests + infrastructure) : product ≈ %.1f : 1 (the C++ referee tool is not Go code and is not counted; auto-updated by CI on push to main)*\n",
		float64(tests.lines+infra.lines)/float64(product.lines))
	return b.String()
}
