//go:build !cgo
// +build !cgo

package czlib

// In non-CGO builds there is no real C zlib to compare against, so all
// entry points return ErrRequiresCGO; tests/tools use HasCGO() to skip the
// C reference leg (the pure Go leg and the C++ native leg are unaffected).

// HasCGO reports whether CGO is enabled.
func HasCGO() bool {
	return false
}

// CompressOpts requires CGO (real C zlib).
func CompressOpts(input []byte, ts uint32, level int, osByte byte) ([]byte, error) {
	return nil, ErrRequiresCGO
}

// CompressWithSyncFlush requires CGO (real C zlib).
func CompressWithSyncFlush(input []byte, ts uint32, level int, osByte byte, splitAt int) ([]byte, error) {
	return nil, ErrRequiresCGO
}

// CompressWithGzHeader requires CGO (the real C zlib deflateSetHeader path).
func CompressWithGzHeader(input []byte, level int, ts uint32, osByte byte,
	extra []byte, name, comment string) ([]byte, error) {
	return nil, ErrRequiresCGO
}
