# Development Principles

The core of this repository is an **algorithm port**: a line-by-line
translation of C zlib's deflate into pure Go (`internal/zdeflate`) whose
compressed output is **byte-for-byte identical** to C zlib. Every
engineering decision serves that goal. Keep it simple (KISS) — no
speculative abstractions, no unnecessary API surface.

## Non-negotiable rules

1. **The C reference sources must never be modified.**
   `internal/czlib/zlib/` is the official zlib 1.3.1 source (only
   `zlib_amalgam.c` is an amalgamated compilation unit added by this
   repository). The cross-checks are only meaningful because the reference
   is pristine — upgrading the version wholesale is fine, editing its
   content is not. CI enforces this: the native job downloads the official
   zlib 1.3.1 tarball (pinned SHA-256), verifies every vendored file is
   byte-identical to it, and builds one referee binary directly from the
   tarball so the "correct answer" never depends on this repository's copy.

2. **The pure Go port must match C's logic exactly.** `internal/zdeflate`
   is a line-by-line counterpart of `deflate.c`/`trees.c`: read the
   corresponding C code before changing any compression logic, and every
   change must pass the full byte-for-byte cross-checks (locally:
   `make test && make native`). Mechanical, output-invariant speedups
   (wide loads, SWAR, the 64-bit bit accumulator) are permitted only at
   proven-hot spots and each must carry a comment proving no output byte
   can change; algorithmic decision points (match acceptance, flush
   decisions, tie-breaks) must remain exactly C's. The single intentional
   byte-level exception is the gzip header XFL byte: zlib's
   deflateSetHeader path writes 2/4/0 depending on level, while this
   library **always writes 0** for stable headers — the comparison code
   fixes up byte 8 before comparing; do not "helpfully" change it to
   follow zlib.

3. **The three-way cross-check, fuzz, and benchmarks are the skeleton of
   this repository and must never be reduced:**
   - `cmd/crossnative -mode check`: C++ native / C reference (cgo) / this
     library, byte-for-byte across levels × flush positions × MTIME × OS ×
     all header parameters × multiple corpora;
   - the crosscheck / stream / fuzz tests in `internal/czlib`;
   - bench: throughput + memory for all sides, auto-written back to the
     README by CI (AUTOBENCH block);
   - loc: the line-count bot (AUTOLOC block).
   When adding compression behavior (new parameters, new flush semantics,
   new header fields), extend these matrices in the same change.

4. **The product API is only the root package's exported surface**:
   `NewWriter`/`NewWriterLevel`/`Writer` (with
   `Header`/`SetHeader`/`Reset`/`Flush`), the `NewReader`/`Reader`
   forwarding, level constants and error values. 100% pure Go, no cgo.
   Do not bring back one-shot APIs (the Writer is everything; the README
   shows equivalent migration code). CGO only affects tests (it decides
   whether the C reference comparison leg is available).

## Layout

| Path | Role |
|---|---|
| `gzip.go` | Product API (the only public package) |
| `internal/zdeflate/` | Product compression implementation (pure Go deflate port) |
| `internal/czlib/` | Test-only C reference (cgo + vendored zlib 1.3.1) |
| `cmd/crossnative/` | Three-way cross-check / bench / README bot orchestrator |
| `native/` | Standalone C++ reference tool and ASan/LSan leak harness |

Tests live next to what they test. There are two cgo gating mechanisms
with different meanings:
- cross-check test files in `internal/czlib` carry `//go:build cgo` — with
  `CGO_ENABLED=0` they are **not compiled at all**; `make test-nocgo` does
  not run those cross-check matrices;
- root-package tests gate at runtime via `czlib.HasCGO()` — non-CGO builds
  skip the C reference leg while the pure Go reference leg (zdeflate)
  always runs.
Full cross-check coverage is defined by `CGO_ENABLED=1` (the cgo side of
`make test` plus `make native`).

## Common commands

```bash
make test        # full test suite in both build modes
make native      # three-way byte-for-byte cross-check
make fuzz        # heavy randomized cross-check
make bench-table # benchmark table
make leak-check  # C-layer ASan/LSan
```

## Contribution flow

Changes go through PRs; wait for the Codex review and judge each of its
comments on the merits (fix what is valid, push back with reasons on what
is not), then merge once CI is green. Note: although ci.yml declares
`on: pull_request`, PRs created by GitHub Apps have been observed to
produce no Actions runs in this repository — before merging, always
trigger ci.yml on the branch manually via `workflow_dispatch` and confirm
it is green; do not be misled by "no failing checks".
