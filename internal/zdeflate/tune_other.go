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

// wideRunThreshold: batch-insert runs with last-first >= this call
// insertRun; shorter runs keep the original scalar insertPos loop
// (0 would disable the call entirely). Two x86 A/B rounds drew the
// boundary (PR #10 round 1, unconditional; PR #12 round 1, threshold 12,
// EPYC 7763): long runs win big — JSON_64KB -12.8%, Large_2KB -5.6%,
// WriterStream -4.3%, all with ~250-position runs — while the 13-23
// position runs of the JSON_1MB family still lost ~1.7%. 32 puts the
// whole measured losing range (and a margin) back on the scalar loop
// while every measured winner (~250-position runs) keeps the wide call.
const wideRunThreshold = 32
