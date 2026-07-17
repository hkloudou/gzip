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
// per-hash triple byte loads bottleneck arm64's load ports. On x86-64
// (Xeon 8370C) the same change was a net +0.99% geomean regression (deep
// OOO already hides the byte loads; the call overhead costs more than the
// load savings), so !arm64 keeps the scalar insertPos loop.
const wideInsertRun = true

// wideSlideTable selects slideTable's two-words-per-iteration loop
// (PR #11): on arm64 every case moved in the right direction — geomean
// -0.87%, level1 -3.68%, JSON_64KB -3.35%, JSON_1MB -1.47%, level6 -1.43%,
// zero regressions (all p<=0.035). On x86-64 (EPYC 7763) the same change
// was a net +0.55% geomean regression dominated by code-layout shifts on
// inputs that never even slide the window (Small_2B +8.9% with slideTable
// unreachable), so !arm64 keeps the one-word loop and its layout.
const wideSlideTable = true

// wideRunThreshold is the minimum last-first for a batch-insert run to
// call insertRun on architectures where wideInsertRun is false. Unused on
// arm64: wideInsertRun short-circuits the condition at compile time, so
// insertRun is always called, exactly as before this constant existed.
const wideRunThreshold = 0
