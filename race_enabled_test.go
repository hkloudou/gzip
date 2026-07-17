//go:build race

package gzip

// raceEnabled reports whether the race detector is on; allocation-count
// assertions are skipped under it (instrumentation changes allocation
// behavior, so the counts are not meaningful there).
const raceEnabled = true
