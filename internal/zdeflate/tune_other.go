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
// insertRun; shorter runs keep the original scalar insertPos loop. The
// PR #10 round-1 x86 data (unconditional insertRun) showed the wide path
// is a real win where runs are long (JSON_64KB -5.79%, ~43 chunks/call)
// but a net loss where they are short-to-medium (JSON_1MB +2.14%, level6
// +2.41%, WriterStream +1.01% on Xeon 8370C). 12 means at least 13
// positions = at least two full 6-wide chunks per call, amortizing the
// call overhead; the A/B verdict on this threshold decides whether it
// ships or non-arm64 reverts to scalar-always (0 disables: with
// wideInsertRun false the condition is then constant-false and this file
// returns to the pre-experiment shape).
const wideRunThreshold = 12
