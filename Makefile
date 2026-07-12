# The product (root package gzip) is 100% pure Go (internal/zdeflate); output
# is identical on every platform and build mode; there is no cgo anywhere in
# this repository. The C reference for the byte-for-byte cross-checks is the
# gzip_ref subprocess referee, built from the OFFICIAL zlib 1.3.1 sources
# (downloaded tarball, pinned SHA-256) — never from C code kept in this repo.

ZLIB_VERSION := 1.3.1
ZLIB_SHA256  := 9a93b2b7dfdac77ceba5a558a580e74667dd6fede4585b91eefb60f03b72df23
# Offline override: point ZLIB131_DIR at any zlib 1.3.1 source tree (must
# contain at least adler32.c crc32.c deflate.c trees.c zutil.c + headers).
ZLIB131_DIR ?=
ZLIB_SRC = $(if $(ZLIB131_DIR),$(ZLIB131_DIR),.cache/zlib-$(ZLIB_VERSION))

.PHONY: test bench bench-table fuzz clean zlib-src native-build native asan-check

# Fetch + verify the official zlib sources (cached under .cache/, gitignored)
zlib-src:
	@if [ -n "$(ZLIB131_DIR)" ]; then \
		test -f "$(ZLIB131_DIR)/deflate.c" || { echo "ZLIB131_DIR has no zlib sources"; exit 1; }; \
	elif [ ! -f "$(ZLIB_SRC)/deflate.c" ]; then \
		mkdir -p .cache; \
		( curl -fsSL --retry 3 -o .cache/zlib-$(ZLIB_VERSION).tar.gz https://zlib.net/zlib-$(ZLIB_VERSION).tar.gz || \
		  curl -fsSL --retry 3 -o .cache/zlib-$(ZLIB_VERSION).tar.gz https://github.com/madler/zlib/releases/download/v$(ZLIB_VERSION)/zlib-$(ZLIB_VERSION).tar.gz ); \
		echo "$(ZLIB_SHA256)  .cache/zlib-$(ZLIB_VERSION).tar.gz" | shasum -a 256 -c -; \
		tar -xzf .cache/zlib-$(ZLIB_VERSION).tar.gz -C .cache; \
	fi

# C++ referee tool: official-zlib build + system-zlib variant. Only the
# deflate-side sources are compiled (no configure needed), so any zlib 1.3.1
# source tree works, including a deflate-only subset.
native-build: zlib-src
	mkdir -p bin/zlibobj
	for f in adler32 crc32 deflate trees zutil; do \
		cc -O2 -c $(ZLIB_SRC)/$$f.c -o bin/zlibobj/$$f.o -I$(ZLIB_SRC) || exit 1; \
	done
	c++ -O2 -std=c++17 native/gzip_ref.cpp bin/zlibobj/*.o -I$(ZLIB_SRC) -o bin/gzip_ref
	c++ -O2 -std=c++17 native/gzip_ref.cpp -lz -o bin/gzip_ref_system || \
		echo "(system zlib unavailable, skipping gzip_ref_system)"

# Byte-for-byte cross-check: official-built native / system-zlib native / pure Go
native: native-build
	go run ./cmd/crossnative -mode check \
		-native $$(test -x bin/gzip_ref_system && echo ./bin/gzip_ref,./bin/gzip_ref_system || echo ./bin/gzip_ref)

# Full test suite. The C-reference legs need bin/gzip_ref (built here);
# without it they skip and only the pure Go legs run.
test: native-build
	go vet ./...
	go test ./...

# Benchmark table (native / pure Go / std Go); optional: BENCHTIME=2s README=README.md
BENCHTIME ?= 1s
bench-table: native-build
	go run ./cmd/crossnative -mode bench -native ./bin/gzip_ref \
		-benchtime $(BENCHTIME) $(if $(README),-update-readme $(README),)

# Pure Go vs std Go micro-benchmarks (for pprof work)
bench:
	go test -bench 'BenchmarkGzip' -benchmem -run '^$$' .

# Heavy fuzz cross-check: official-zlib referee vs pure Go, byte-for-byte
# Usage: make fuzz [ITER=2000] [MAXSIZE=2097152] [SEED=1]
ITER    ?= 1000
MAXSIZE ?= 1048576
SEED    ?= 1
fuzz: native-build
	ZLIB_FUZZ_ITER=$(ITER) ZLIB_FUZZ_MAXSIZE=$(MAXSIZE) ZLIB_FUZZ_SEED=$(SEED) \
		go test . -run TestCrossCheckFuzz -timeout 3600s -v

# ASan/LSan over every gzip_ref mode (compress/stream/header/bench), the
# whole C surface this repository executes
asan-check: zlib-src
	mkdir -p bin/zlibobj-asan
	for f in adler32 crc32 deflate trees zutil; do \
		cc -O1 -g -fsanitize=address,leak -c $(ZLIB_SRC)/$$f.c -o bin/zlibobj-asan/$$f.o -I$(ZLIB_SRC) || exit 1; \
	done
	c++ -O1 -g -fsanitize=address,leak -std=c++17 native/gzip_ref.cpp bin/zlibobj-asan/*.o -I$(ZLIB_SRC) -o bin/gzip_ref_asan
	./native/asan_sweep.sh ./bin/gzip_ref_asan

clean:
	go clean ./...
	rm -rf bin .cache
