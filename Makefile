# The product (root package gzip) is 100% pure Go (internal/zdeflate); output
# is identical under any build mode. CGO only affects tests: CGO_ENABLED=1
# compiles the embedded zlib 1.3.1 (internal/czlib) as the real C reference
# for byte-for-byte cross-checking.

.PHONY: test test-cgo test-nocgo bench bench-table fuzz clean \
	native-build native leak-check

# C++ native reference tool (real zlib, no cgo) — vendored + system zlib variants
native-build:
	mkdir -p bin
	cc  -O2 -c internal/czlib/zlib/zlib_amalgam.c -o bin/zlib_amalgam.o -Iinternal/czlib/zlib
	c++ -O2 -std=c++17 native/gzip_ref.cpp bin/zlib_amalgam.o -Iinternal/czlib/zlib -o bin/gzip_ref
	c++ -O2 -std=c++17 native/gzip_ref.cpp -lz -o bin/gzip_ref_system || \
		echo "(system zlib unavailable, skipping gzip_ref_system)"

# Three-way byte-for-byte cross-check: C++ native / Go+cgo / pure Go
native: native-build
	CGO_ENABLED=1 go run ./cmd/crossnative -mode check \
		-native $$(test -x bin/gzip_ref_system && echo ./bin/gzip_ref,./bin/gzip_ref_system || echo ./bin/gzip_ref)

# Three-way benchmark, renders a markdown table (optional: BENCHTIME=2s README=README.md)
BENCHTIME ?= 1s
bench-table: native-build
	CGO_ENABLED=1 go run ./cmd/crossnative -mode bench -native ./bin/gzip_ref \
		-benchtime $(BENCHTIME) $(if $(README),-update-readme $(README),)

# Precise ASan/LSan leak check for the C layer
leak-check:
	mkdir -p bin
	cc -O1 -g -fsanitize=address,leak native/leak_check.c -Iinternal/czlib/zlib -o bin/leak_check
	./bin/leak_check

test: test-cgo test-nocgo

# CGO tests (includes byte-for-byte cross-check of real C zlib vs this library)
test-cgo:
	CGO_ENABLED=1 go test ./...

# Pure Go mode tests
test-nocgo:
	CGO_ENABLED=0 go test ./...

# C reference vs this library's pure Go performance comparison (both sets in one run)
bench:
	CGO_ENABLED=1 go test -bench 'BenchmarkGzip' -benchmem -run '^$$' ./internal/czlib/

# Heavy fuzz cross-check: C zlib vs pure Go, byte-for-byte comparison
# Usage: make fuzz [ITER=2000] [MAXSIZE=2097152] [SEED=1]
ITER    ?= 1000
MAXSIZE ?= 1048576
SEED    ?= 1
fuzz:
	ZLIB_FUZZ_ITER=$(ITER) ZLIB_FUZZ_MAXSIZE=$(MAXSIZE) ZLIB_FUZZ_SEED=$(SEED) \
		CGO_ENABLED=1 go test ./internal/czlib/ -run TestCrossCheckFuzz -timeout 3600s -v

clean:
	go clean ./...
