//go:build arm64

package zdeflate

// Per-architecture tuning constants. Each gate only picks between two
// proven byte-identical code paths — output bytes never depend on them.
// Every value is decided by A/B CI data (10 interleaved rounds, benchstat,
// x86-64 + arm64 runners), never by local-container numbers; the PR
// referenced on each constant carries the measurements.
//
// wideInsertRun selects insertRun's 6-positions-per-load wide path for the
// deflateSlow batch inserts (PR #10): on arm64 (Cobalt 100) the wide path
// is a -10.16% sec/op geomean — JSON_64KB -37.3%, WriterStream -24.2%,
// Large_2KB -21.2%, JSON_1MB -9.3%, no regressions (all p=0.000) — the
// per-hash triple byte loads bottleneck arm64's load ports. !arm64 keeps
// the scalar insertPos loop: CLOSED DECISION (2026-07, PRs #10/#12 — do
// not re-litigate without new data). Three x86 A/B rounds landed on three
// microarchitectures and disagreed on the sign — unconditional on Xeon
// 8370C +0.99% geomean; run-length threshold 12 on EPYC 7763 -1.18%
// (JSON_64KB -12.8%); threshold 32 on EPYC 9V74 +1.43% (the same
// JSON_64KB win shrank to -2.5%, the JSON_1MB family regressed +2.8%) —
// with +3..13% binary-layout phantoms on inputs that never reach the
// branch in every round. The x86 benefit is microarch-dependent and not
// reproducible across GitHub's runner pool (or user machines); arm64's
// win reproduced on the same Cobalt hardware every round.
const wideInsertRun = true

// wideSlideTable selects slideTable's two-words-per-iteration loop
// (PR #11): on arm64 every case moved in the right direction — geomean
// -0.87%, level1 -3.68%, JSON_64KB -3.35%, JSON_1MB -1.47%, level6 -1.43%,
// zero regressions (all p<=0.035). On x86-64 (EPYC 7763) the same change
// was a net +0.55% geomean regression dominated by code-layout shifts on
// inputs that never even slide the window (Small_2B +8.9% with slideTable
// unreachable), so !arm64 keeps the one-word loop and its layout.
const wideSlideTable = true
