# gzip — byte-identical-to-zlib GZIP compression for Go

[![CI](https://github.com/hkloudou/gzip/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/hkloudou/gzip/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/hkloudou/gzip.svg)](https://pkg.go.dev/github.com/hkloudou/gzip)
![Go Version](https://img.shields.io/badge/go-%3E%3D1.21-blue)
![Pure Go](https://img.shields.io/badge/100%25-Pure%20Go-success)

Go's standard `compress/gzip` (backed by `compress/flate`) implements the
compression algorithm differently from C zlib: **the same input produces
different compressed bytes**. Meanwhile iOS, Java (OpenJDK) and most other
ecosystems ship GZIP built on standard zlib. Whenever a system needs
byte-level comparison of compressed output (signatures, cache keys,
deduplication, incremental diffs), Go is the odd one out.

This library produces GZIP output **byte-identical to zlib**. The product is
**100% pure Go** (a line-by-line port of zlib's deflate, `internal/zdeflate`):
no cgo, no external dependencies, cross-compiles anywhere (including
js/wasm), and emits exactly the same bytes on every platform and build mode.
In fact the repository contains **no zlib C code at all** — the reference
implementation used by tests is real zlib built from the **official 1.3.1
tarball** (pinned SHA-256), driven as a separate process.

Consistency is enforced by three layers of test infrastructure (never part
of the product build — tests/CI only):

| Reference | Location | Purpose |
|---|---|---|
| `gzip_ref` referee | `native/gzip_ref.cpp` (subprocess; a thin driver with no compression logic) | Real zlib as the deterministic correct answer: built from the official zlib 1.3.1 tarball (the pinned byte-correctness referee), plus official-1.3.2 and system-zlib variants |
| Cross-check / fuzz / streaming matrices | root package `*_ref_test.go` | Levels 0-9 × large corpora × flush sequences × all header parameters × random fuzz, byte-compared against the referee (`make test` / `make fuzz`) |
| crossnative orchestrator | `cmd/crossnative` | The CI cross-check matrix, the benchmark table and the line-count bot |

## Usage

### 1. Drop-in migration from the standard library

```go
import gzip "github.com/hkloudou/gzip"

func compress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf) // or gzip.NewWriterLevel(&buf, 9)
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil { // must Close, or the stream is incomplete
		return nil, err
	}
	return buf.Bytes(), nil
}
```

Behavior is fully aligned with C zlib (whatever C allows, this library
allows, with identical bytes):

- `Write` = `deflate(Z_NO_FLUSH)`: streaming incremental compression, the
  input is never buffered in full. zlib guarantees that under `Z_NO_FLUSH`
  the output is independent of how the input is sliced (level > 0), so
  without Flush the output is byte-identical to one-shot compression;
- `Flush` = `deflate(Z_SYNC_FLUSH)`: terminates the current block and emits
  the `00 00 FF FF` sync marker, exactly like C. Flush positions become part
  of the output bytes (same in C) — when comparing across languages either
  neither side flushes or both flush at the same offsets;
- `Close` = `deflate(Z_FINISH)` + the GZIP trailer.

Differences from the standard library (all in service of matching zlib
output):

- Compression levels follow zlib: `-1` is zlib's `Z_DEFAULT_COMPRESSION`
  (internally 6; Java's `Deflater.DEFAULT_COMPRESSION` and Go's
  `gzip.DefaultCompression` are also -1), plus `0`-`9`; the stdlib-specific
  `HuffmanOnly(-2)` is not supported;
- Header defaults are `OS=3` (Unix), `XFL=0`, `MTIME=0` (stdlib defaults to
  `OS=255` and writes `XFL=4/2` at levels 1/9; zlib's `deflateSetHeader`
  path also writes level-dependent XFL — this library deliberately pins XFL
  to 0 for stable headers across all levels; XFL is a hint field and does
  not affect decompression);
- The extra `writer.Mtime` (uint32) is a design specific to this library:
  it sets the raw GZIP MTIME seconds directly and takes priority over
  `ModTime` when non-zero, bypassing `time.Time` conversions;
- Decompression (`NewReader`/`Reader`) forwards to the standard library
  (decompression has no byte-consistency problem).

### 2. Customizing the header

`Header` has exactly the same field set as the standard library's
`compress/gzip.Header` (`Comment`/`Extra`/`ModTime`/`Name`/`OS`), written
per RFC 1952 as FEXTRA/FNAME/FCOMMENT. All header parameters are set
directly on the `Writer` (the one-shot API has been removed — the Writer is
everything; the equivalent of the old `zlib.Compress(data, ts)` is shown
below):

```go
var buf bytes.Buffer
w := gzip.NewWriterLevel(&buf, 6)   // level -1, 0-9, same as zlib
w.Mtime = 1751038273                // library-specific: raw MTIME seconds
                                    // (wins over ModTime when non-zero)
w.ModTime = time.Now()              // or the stdlib way (pre-1970 writes 0)
w.OS = 3                            // default 3 (Unix), customizable
w.Name = "data.json"                // FNAME (NUL-terminated Latin-1, as stdlib)
w.Comment = "naïve café"            // FCOMMENT
w.Extra = []byte{0xde, 0xad}        // FEXTRA (≤65535 bytes)
w.Write(data)
w.Close()
```

With all optional fields zero (only Mtime set) the output layout is:

```
[10-byte header: 1f 8b 08 00 <mtime LE> 00 <os>][raw deflate][crc32 LE][isize LE]
```

These fields are cross-checked byte-for-byte against real C zlib's
`deflateSetHeader` (`gz_header`) output (CI full-parameter matrix: field
combinations × OS × MTIME × level × multiple corpora); the only deliberate
difference is the XFL byte (see above).

Note: because of the extra `Mtime` field, this library's `Header` is a
different type from the stdlib's — field-by-field assignment is compatible,
whole-struct assignment (`zw.Header = zr.Header`) does not compile. Use
`w.SetHeader(r.Header)` for recompression flows that carry headers over.

Migrating from the removed one-shot API (byte-identical output):

```go
// formerly zlib.Compress(data, timestamp) / zlib.CompressLevel(data, ts, 9)
func compress(data []byte, ts uint32, level int) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		return nil, err
	}
	w.Mtime = ts
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
```

## Byte-level zlib compatibility across platforms

| Platform | Byte-identical to standard zlib? | Notes |
|---|---|---|
| zlib 1.3.2 (newest official release) | ✅ identical | **Verified in CI every run**: a second referee built from the official 1.3.2 tarball joins the full cross-check matrix; 1.3.2's changes (build system, hardening, inflate, new `_z` APIs) do not touch deflate output |
| iOS/macOS system libz (`deflate()`) | ✅ identical | **Verified in CI every run**: the macOS native job links the referee against the system libz (`gzip_ref_system`) and cross-checks all levels/matrices byte-for-byte against this library. Apple's changes are limited to inflate/checksum acceleration; the gzip header OS byte is 19 on Apple platforms, which this library sidesteps by writing OS=3 itself |
| Apple Compression framework (`COMPRESSION_ZLIB`) | ⚠️ measured = zlib level 9, not guaranteed | Apple only promises raw DEFLATE and describes it as a "level 5 equivalent"; measurements (macOS 15.7 arm64, v1.1.x CI probe) showed its output byte-identical to standard zlib **level 9** (not 5). This is an unpromised implementation detail that may change with OS versions |
| Java `java.util.zip.Deflater` (OpenJDK) | ✅ identical | Bundles/links official zlib (except distros like Fedora 40+ that switched to zlib-ng) |
| Android 13+ | ✅ identical | Chromium fork with the zlib-compatible hash enabled (fixed for OTA incremental updates) |
| Android 11–12L | ❌ different | The Chromium fork's CRC32C hash produces valid but non-canonical output |
| Chrome's bundled zlib | ❌ different | Non-canonical hash by default |
| Go `compress/gzip` | ❌ different | Its own flate implementation — the reason this library exists |

## Testing and performance

```bash
make test        # referee build + full test suite (C legs skip if no C toolchain)
make native      # byte-for-byte cross-check matrix (official 1.3.1 + 1.3.2 + system zlib referees)
make fuzz        # heavy randomized cross-check (real zlib vs pure Go)
make bench-table # benchmark table (C++ zlib 1.3.1 + 1.3.2 / pure Go / std Go)
make asan-check  # ASan/LSan sweep over every referee mode (both official builds)
```

The referees are built from the official `zlib-1.3.1.tar.gz` and
`zlib-1.3.2.tar.gz` (downloaded and SHA-256-verified into `.cache/`, with a
`zlib.net/fossils/` fallback for releases zlib.net has rotated out; or point
`ZLIB131_DIR`/`ZLIB132_DIR` at existing zlib source trees for offline work).
Every push runs
[GitHub Actions](.github/workflows/ci.yml) as the final consistency backstop
(click the badge above for live results). For optimization work there is
also an on-demand A/B workflow ([abbench.yml](.github/workflows/abbench.yml),
`workflow_dispatch`): any branch vs a baseline ref, interleaved sampling +
benchstat, on free x86-64 and arm64 runners — CI hardware is the measuring
stick, not developer containers:

| CI job | Coverage |
|---|---|
| test (ubuntu x64 + arm64/macos/windows) | Full unit tests + real-zlib cross-checks: levels 0-9 × flush types (NO_FLUSH/PARTIAL/SYNC/FULL/FINISH) full matrix, golden vectors, streaming call sequences, Writer behavior, **all header parameters × real zlib `deflateSetHeader`** (deterministic + seeded random fuzz), streaming-output/HTTP tests, `sync.Pool` stress. On windows the referee legs skip (pure Go coverage only) |
| native (ubuntu x64 + arm64/macos) | **Cross-check matrix**: real zlib vs pure Go, byte-for-byte on real x86-64, arm64 and Apple hardware, against three referees — the **official zlib 1.3.1 tarball** (pinned SHA-256; the byte-correctness pin, independent of this repository), the **official zlib 1.3.2 tarball** (the newest release, same pinning), and the system zlib — on macOS that is Apple's libz, so Apple-platform compatibility is re-verified every run. Matrix: 8 corpora × 11 levels × flush positions + streaming call sequences + MTIME × OS dimensions + all header parameters + empty input |
| race (x64 + arm64) | Full test suite (referee included) under the Go race detector, with dedicated `sync.Pool` adversarial tests (concurrent cross-contamination, scratch aliasing, chunk-boundary stress, Writer reuse); the arm64 leg exercises the weaker memory ordering x86 hides |
| sanitize | ASan + LeakSanitizer over every referee mode (compress/stream/header/bench × parameter edge cases), for both official zlib builds (1.3.1 + 1.3.2) |
| fuzz | 500 random inputs × random levels, real zlib vs pure Go byte comparison |
| bench | Four-way benchmark (C++ zlib 1.3.1 / C++ zlib 1.3.2 / pure Go / std Go, with memory stats); auto-updates the table below on push to main. A second arm64 run goes to the job summary (regression watch for arm64) |
| cross-build | Pure-Go cross-compilation for linux/arm64, windows, darwin/arm64, js/wasm |

### Benchmark (auto-updated by CI)

<!-- AUTOBENCH:BEGIN -->
Level 6, each op is a full compression (reset + deflate + CRC + gzip framing); every column reuses compressor state (C++ via deflateReset, the Go columns via sync.Pool / Writer.Reset).

- **C++ zlib 1.3.1**: real zlib built from the official 1.3.1 sources, looping in-process — the C performance ceiling and the byte-correctness referee
- **C++ zlib 1.3.2**: real zlib built from the official 1.3.2 sources, looping in-process — the newest official zlib release (byte-identical output, enforced by the cross-check matrix)
- **Pure Go**: this library
- **Std Go**: the standard library compress/gzip — speed context only; its output bytes differ by design, which is the reason this library exists

**Speed** (ratios are relative speed of Pure Go; higher = Pure Go faster):

| Input | C++ zlib 1.3.1 | C++ zlib 1.3.2 | Pure Go | Std Go | Pure Go / C++ zlib 1.3.1 | Pure Go / C++ zlib 1.3.2 | Pure Go / Std Go |
|---|---|---|---|---|---|---|---|
| 2 B | 1.7 µs/op | 1.8 µs/op | 2.7 µs/op | 11.3 µs/op | 0.64× | 0.67× | **4.14× faster** |
| 198 B JSON token | 7.4 µs/op | 7.8 µs/op | 8.0 µs/op | 20.9 µs/op | 0.92× | 0.97× | **2.60× faster** |
| 2 KB JSON | 15.1 µs/op | 11.6 µs/op | 8.6 µs/op | 20.0 µs/op | **1.76× faster** | **1.35× faster** | **2.33× faster** |
| 64 KB JSON | 309.0 µs (212 MB/s) | 310.5 µs (211 MB/s) | 162.8 µs (403 MB/s) | 182.4 µs (359 MB/s) | **1.90× faster** | **1.91× faster** | 1.12× |
| 1 MB JSON | 10.1 ms (104 MB/s) | 10.3 ms (102 MB/s) | 7.4 ms (141 MB/s) | 7.1 ms (148 MB/s) | **1.36× faster** | **1.38× faster** | 0.95× |
| 1 MB random (incompressible) | 23.9 ms (44 MB/s) | 24.1 ms (44 MB/s) | 21.2 ms (49 MB/s) | 18.3 ms (57 MB/s) | 1.13× | 1.13× | 0.86× |

**Memory** (Go heap per op; the native referee is a subprocess and has no Go heap; Std Go compresses into a reused bytes.Buffer while Pure Go returns a fresh exact-size slice per op):

| Input | Pure Go | Std Go |
|---|---|---|
| 2 B | 24 B · 1 allocs | 0 B · 0 allocs |
| 198 B JSON token | 208 B · 1 allocs | 0 B · 0 allocs |
| 2 KB JSON | 100 B · 1 allocs | 0 B · 0 allocs |
| 64 KB JSON | 315 B · 1 allocs | 0 B · 0 allocs |
| 1 MB JSON | 170.6 KB · 1 allocs | 0 B · 0 allocs |
| 1 MB random (incompressible) | 1.1 MB · 2 allocs | 0 B · 0 allocs |

*2026-07-12 18:29 UTC · AMD EPYC 7763 64-Core Processor · go 1.26.5 · linux/amd64 · commit `4e06155` (auto-updated by CI on push to main)*
<!-- AUTOBENCH:END -->

The standard-library column is performance-only context — its output bytes
differ by design, which is the reason this library exists.

### Code size (auto-counted by CI)

<!-- AUTOLOC:BEGIN -->
| Category | Files | Go lines |
|---|---|---|
| Product (root package + internal/zdeflate, pure Go) | 5 | 2355 |
| Tests (*_test.go) | 8 | 2215 |
| Test infrastructure (cmd/crossnative, non-test) | 1 | 851 |

*(tests + infrastructure) : product ≈ 1.3 : 1 (the C++ referee tool is not Go code and is not counted; auto-updated by CI on push to main)*
<!-- AUTOLOC:END -->

## Memory notes

- Compressor state (window/hash tables/symbol buffers, ~320KB measured) is
  reused via `sync.Pool` — one-shot compression allocates exactly once (the
  result slice). Pool correctness gets dedicated adversarial tests
  (concurrent cross-contamination, scratch aliasing, chunk-boundary stress,
  Writer reuse) run under the race detector in CI;
- `gzip.Writer` is truly streaming: `Write` compresses incrementally and
  pushes downstream without buffering the whole input (O(1) memory for
  large files); internal output scratch is pooled too, and the steady-state
  streaming cycle (`Reset`/`Write`/`Flush`/`Close`) allocates **zero** bytes
  per stream (pinned by `TestSteadyStateAllocs` in CI). `Flush` makes
  everything written so far immediately decodable (Z_SYNC_FLUSH), which is
  what HTTP/SSE streaming needs — covered by dedicated streaming-output and
  httptest end-to-end tests;
- The C surface executed by tests (the gzip_ref referee) runs under precise
  ASan/LeakSanitizer sweeps in CI across every mode and parameter edge case.
