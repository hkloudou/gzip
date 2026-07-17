# Agent Instructions

This file guides AI coding agents (OpenAI Codex and anything else that
reads `AGENTS.md`) working on this repository. Claude Code reads the
equivalent `CLAUDE.md`; keep the two consistent when rules change. Full
detail lives in `CLAUDE.md` — the hard rules, summarized:

## Hard rules

1. The product is a line-by-line pure Go port of C zlib's deflate
   (`internal/zdeflate`) whose compressed output must stay **byte-for-byte
   identical to C zlib**. Algorithmic decision points (match acceptance,
   flush decisions, tie-breaks, the hash function) must remain exactly
   C's; mechanical speedups are allowed only at proven-hot spots and each
   must carry a comment proving no output byte can change.
2. **No zlib C code in the repository, ever.** The referees are built from
   the official zlib tarballs (versions + SHA-256 pinned in the Makefile)
   and driven as subprocesses; `native/gzip_ref.cpp` is a thin driver with
   no compression logic.
3. **100% pure Go**: no cgo anywhere, not even in tests.
4. The cross-check / fuzz / benchmark / allocation matrices are the
   skeleton of the repository and **must never be reduced**.
5. Verification is `make test && make native` (byte-for-byte vs official
   zlib 1.3.1 + 1.3.2 + system referees). Performance claims are measured
   on CI hardware with the A/B workflow (`abbench.yml`), not on developer
   containers.

## Code review behavior

When reviewing a pull request in this repository:

- **Always post a summary comment with your verdict.** If you found no
  issues, post a brief comment such as "Codex review: no findings — LGTM"
  instead of only reacting with 👍. Rationale: a comment generates a
  webhook event that downstream review automation receives in real time; a
  reaction generates no event at all.
- Prioritize findings in this order:
  1. anything that could change a compressed output byte — including
     subtle Go/C semantic differences (integer widths, unsigned
     wraparound, masking, shift semantics) in `internal/zdeflate`;
  2. missing or weakened verification: reduced matrices, skipped referee
     legs, loosened test assertions;
  3. `sync.Pool` lifecycle bugs: cross-contamination between reuses,
     results aliasing pooled scratch, state escaping the pool;
  4. everything else (style, docs, CI plumbing).
- Flag any change to compression logic that lacks a byte-invariance proof
  comment, and any reduction of a test matrix, even when the code looks
  correct.
