//go:build !arm64

package zdeflate

// Per-architecture tuning constants for every non-arm64 target — see
// tune_arm64.go for the A/B data behind each split. The compile-time
// false eliminates the gated branch entirely, so these targets keep the
// original loops and their original code layout.
const (
	wideInsertRun  = false // deflateSlow batch inserts: inline insertPos loop
	wideSlideTable = false // slideTable: one word per iteration
)

// wideRunThreshold: batch-insert runs with last-first >= this would call
// insertRun; 0 disables the call entirely (the condition constant-folds
// away, leaving the original scalar loop and codegen). CLOSED DECISION
// (2026-07, PRs #10/#12 — do not re-litigate without new data): three
// A/B rounds landed on three different x86 microarchitectures and
// disagreed on the sign — unconditional on Xeon 8370C was +0.99% geomean,
// threshold 12 on EPYC 7763 was -1.18% (JSON_64KB -12.8%), threshold 32
// on EPYC 9V74 was +1.43% (the same JSON_64KB win shrank to -2.5% and the
// JSON_1MB family regressed +2.8%), with binary-layout phantoms of
// +3..13% on inputs that never reach this branch in every round. The
// wide-path benefit on x86 is microarch-dependent and not reproducible
// across GitHub's runner pool (or user machines), so non-arm64 keeps the
// scalar loop; the reproducible wide-path win ships on arm64 only
// (wideInsertRun, measured on the same Cobalt hardware every round).
const wideRunThreshold = 0
