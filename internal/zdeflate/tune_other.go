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
