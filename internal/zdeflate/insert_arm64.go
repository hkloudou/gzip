//go:build arm64

package zdeflate

// wideInsertRun selects insertRun's 6-positions-per-load wide path for the
// deflateSlow batch inserts. arm64-only, from A/B CI data (10 interleaved
// rounds, benchstat, PR #10): on arm64 (Cobalt 100) the wide path is a
// -10.47% sec/op geomean — JSON_64KB -37.3%, WriterStream -24.2%,
// Large_2KB -21.2%, JSON_1MB -9.4%, no regressions (all p=0.000) — the
// per-hash triple byte loads bottleneck arm64's load ports. On x86-64
// (Xeon 8370C) the same change was a net +0.99% geomean regression (deep
// OOO already hides the byte loads; the call overhead costs more than the
// load savings), so !arm64 keeps the scalar insertPos loop. Output bytes
// are identical on every path — the gate only picks between two proven
// byte-identical loops.
const wideInsertRun = true
