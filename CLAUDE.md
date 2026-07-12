# Development Principles

The core of this repository is an **algorithm port**: a line-by-line
translation of C zlib's deflate into pure Go (`internal/zdeflate`) whose
compressed output is **byte-for-byte identical** to C zlib. Every
engineering decision serves that goal. Keep it simple (KISS) — no
speculative abstractions, no unnecessary API surface.

## Non-negotiable rules

1. **No zlib C code lives in this repository, ever.** The C reference is
   the `gzip_ref` referee binary, built from the **official zlib 1.3.1
   tarball** (downloaded with a pinned SHA-256 by `make zlib-src`, or an
   offline source tree via `ZLIB131_DIR`) and driven as a subprocess.
   `native/gzip_ref.cpp` is a thin driver with no compression logic — do
   not vendor, patch, or re-implement zlib in C here; the "correct answer"
   must always come from the unmodified official release. Upgrading means
   changing the pinned version+hash in the Makefile, nothing else.

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

3. **The cross-check matrices, fuzz, and benchmarks are the skeleton of
   this repository and must never be reduced:**
   - `cmd/crossnative -mode check`: official-build referee + system-zlib
     referee vs this library, byte-for-byte across levels × flush
     positions × streaming call sequences × MTIME × OS × all header
     parameters × multiple corpora;
   - the `*_ref_test.go` crosscheck / stream / header / fuzz matrices and
     the streaming-output + HTTP tests in the root package;
   - bench: throughput + memory (C++ native / pure Go / std Go),
     auto-written back to the README by CI (AUTOBENCH block);
   - loc: the line-count bot (AUTOLOC block).
   When adding compression behavior (new parameters, new flush semantics,
   new header fields), extend these matrices in the same change.

4. **The product API is only the root package's exported surface**:
   `NewWriter`/`NewWriterLevel`/`Writer` (with
   `Header`/`SetHeader`/`Reset`/`Flush`), the `NewReader`/`Reader`
   forwarding, level constants and error values. 100% pure Go; there is no
   cgo anywhere in the repository (not even in tests). Do not bring back
   one-shot APIs (the Writer is everything; the README shows equivalent
   migration code).

## Layout

| Path | Role |
|---|---|
| `gzip.go` | Product API (the only public package) |
| `internal/zdeflate/` | Product compression implementation (pure Go deflate port) |
| `cmd/crossnative/` | Cross-check / bench / README bot orchestrator |
| `native/` | C++ referee driver (`gzip_ref.cpp`) and the ASan sweep script |

Tests live next to what they test, in the root package. C-reference test
legs gate at runtime on the referee binary (`bin/gzip_ref`, or the
`ZLIB_REF` env var): when it is absent they skip and only the pure Go legs
run — CI always builds the referee on ubuntu/macos, so skips cannot hide a
regression there. Full cross-check coverage is defined by `make test`
(which builds the referee first) plus `make native`.

## Common commands

```bash
make test        # referee build + full test suite
make native      # cross-check matrix (official + system referee vs pure Go)
make fuzz        # heavy randomized cross-check
make bench-table # benchmark table
make asan-check  # ASan/LSan sweep over every referee mode
```

The referee build downloads the official tarball into `.cache/`
(SHA-256-pinned). Offline, point `ZLIB131_DIR` at any zlib 1.3.1 source
tree (it needs at least adler32/crc32/deflate/trees/zutil + headers).

## Contribution flow

Changes go through PRs; wait for the Codex review and judge each of its
comments on the merits (fix what is valid, push back with reasons on what
is not), then merge once CI is green. App-created PRs have been observed
both to trigger and not to trigger `pull_request` workflow runs — if no
run appears, trigger ci.yml on the branch via `workflow_dispatch`; either
way, confirm a green run for the branch's HEAD commit before merging and
do not be misled by "no failing checks". Releases (tag pushes) go through
release.yml (`workflow_dispatch` with a `tag` input) and only on the
owner's explicit instruction.
