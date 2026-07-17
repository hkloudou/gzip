//go:build !arm64

package zdeflate

// Scalar batch inserts for every non-arm64 architecture — see
// insert_arm64.go for the A/B data behind the split. The compile-time
// false eliminates the insertRun branch entirely, so the deflateSlow
// batch site keeps its original inline insertPos loop (and its original
// code layout) on these targets.
const wideInsertRun = false
