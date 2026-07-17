# The product (root package gzip) is 100% pure Go (internal/zdeflate); output
# is identical on every platform and build mode; there is no cgo anywhere in
# this repository. The C reference for the byte-for-byte cross-checks is the
# gzip_ref subprocess referee, built from OFFICIAL zlib tarballs (downloaded,
# pinned SHA-256) — never from C code kept in this repo. Two official builds:
# 1.3.1 (the pinned byte-correctness referee) and 1.3.2 (the newest official
# release, cross-checked and benchmarked alongside it).

ZLIB131_VERSION := 1.3.1
ZLIB131_SHA256  := 9a93b2b7dfdac77ceba5a558a580e74667dd6fede4585b91eefb60f03b72df23
ZLIB132_VERSION := 1.3.2
ZLIB132_SHA256  := bb329a0a2cd0274d05519d61c667c062e06990d72e125ee2dfa8de64f0119d16
# Offline overrides: point ZLIB131_DIR / ZLIB132_DIR at matching zlib source
# trees (must contain at least adler32.c crc32.c deflate.c trees.c zutil.c +
# headers).
ZLIB131_DIR ?=
ZLIB132_DIR ?=
ZLIB131_SRC = $(if $(ZLIB131_DIR),$(ZLIB131_DIR),.cache/zlib-$(ZLIB131_VERSION))
ZLIB132_SRC = $(if $(ZLIB132_DIR),$(ZLIB132_DIR),.cache/zlib-$(ZLIB132_VERSION))

# Only the deflate-side sources are compiled (no configure needed), so any
# matching zlib source tree works, including a deflate-only subset.
ZLIB_OBJS := adler32 crc32 deflate trees zutil

.PHONY: test test-lowmem bench bench-table fuzz clean zlib-src zlib-src-132 native-build native-build-132 native asan-check

# Fetch + verify an official zlib source tarball (cached under .cache/,
# gitignored). zlib.net moves superseded releases to fossils/, hence the
# fallback chain: current URL → fossils → GitHub release asset.
# $(call fetch-zlib,<dir-override>,<version>,<sha256>)
define fetch-zlib
	if [ -n "$(1)" ]; then \
		test -f "$(1)/deflate.c" || { echo "zlib $(2) override dir has no zlib sources"; exit 1; }; \
	elif [ ! -f ".cache/zlib-$(2)/deflate.c" ]; then \
		mkdir -p .cache && \
		( curl -fsSL --retry 3 -o .cache/zlib-$(2).tar.gz https://zlib.net/zlib-$(2).tar.gz || \
		  curl -fsSL --retry 3 -o .cache/zlib-$(2).tar.gz https://zlib.net/fossils/zlib-$(2).tar.gz || \
		  curl -fsSL --retry 3 -o .cache/zlib-$(2).tar.gz https://github.com/madler/zlib/releases/download/v$(2)/zlib-$(2).tar.gz ) && \
		echo "$(3)  .cache/zlib-$(2).tar.gz" | shasum -a 256 -c - && \
		tar -xzf .cache/zlib-$(2).tar.gz -C .cache; \
	fi
endef

zlib-src:
	@$(call fetch-zlib,$(ZLIB131_DIR),$(ZLIB131_VERSION),$(ZLIB131_SHA256))

zlib-src-132:
	@$(call fetch-zlib,$(ZLIB132_DIR),$(ZLIB132_VERSION),$(ZLIB132_SHA256))

# Build gzip_ref against a zlib source tree.
# $(call build-referee,<zlib-src-dir>,<obj-subdir>,<output-binary>)
define build-referee
	mkdir -p bin/$(2)
	for f in $(ZLIB_OBJS); do \
		cc -O2 -c $(1)/$$f.c -o bin/$(2)/$$f.o -I$(1) || exit 1; \
	done
	c++ -O2 -std=c++17 native/gzip_ref.cpp bin/$(2)/*.o -I$(1) -o bin/$(3)
endef

# C++ referee tool: official zlib 1.3.1 build (the pinned byte-correctness
# referee, used by the test suite) + system-zlib variant.
native-build: zlib-src
	$(call build-referee,$(ZLIB131_SRC),zlibobj,gzip_ref)
	c++ -O2 -std=c++17 native/gzip_ref.cpp -lz -o bin/gzip_ref_system || \
		echo "(system zlib unavailable, skipping gzip_ref_system)"

# Additional referee from the official zlib 1.3.2 tarball (cross-check leg +
# benchmark column; the byte-correctness pin stays 1.3.1).
native-build-132: zlib-src-132
	$(call build-referee,$(ZLIB132_SRC),zlibobj-132,gzip_ref_132)

# Byte-for-byte cross-check: official 1.3.1 / official 1.3.2 / system-zlib
# natives vs pure Go
native: native-build native-build-132
	go run ./cmd/crossnative -mode check \
		-native $$(test -x bin/gzip_ref_system && echo ./bin/gzip_ref,./bin/gzip_ref_132,./bin/gzip_ref_system || echo ./bin/gzip_ref,./bin/gzip_ref_132)

# Full test suite. The C-reference legs need bin/gzip_ref (built here);
# without it they skip and only the pure Go legs run.
test: native-build
	go vet ./...
	go test ./...

# Benchmark table (native 1.3.1 + 1.3.2 / pure Go / std Go);
# optional: BENCHTIME=2s README=README.md
BENCHTIME ?= 1s
bench-table: native-build native-build-132
	go run ./cmd/crossnative -mode bench -native ./bin/gzip_ref,./bin/gzip_ref_132 \
		-benchtime $(BENCHTIME) $(if $(README),-update-readme $(README),)

# Pure Go vs std Go micro-benchmarks (for pprof work)
bench:
	go test -bench 'BenchmarkGzip' -benchmem -run '^$$' .

# Byte parity + full tests for the low-memory build option (-tags
# gziplowmem: C's sym_buf/pending_buf overlay, 48KB less state per
# compressor; the default build stays speed-first — see CLAUDE.md).
# Same referee set as `native`: official 1.3.1 + 1.3.2 + system zlib.
test-lowmem: native-build native-build-132
	go vet -tags gziplowmem ./...
	go test -tags gziplowmem ./...
	go run -tags gziplowmem ./cmd/crossnative -mode check \
		-native $$(test -x bin/gzip_ref_system && echo ./bin/gzip_ref,./bin/gzip_ref_132,./bin/gzip_ref_system || echo ./bin/gzip_ref,./bin/gzip_ref_132)

# Heavy fuzz cross-check: official-zlib referee vs pure Go, byte-for-byte
# Usage: make fuzz [ITER=2000] [MAXSIZE=2097152] [SEED=1]
ITER    ?= 1000
MAXSIZE ?= 1048576
SEED    ?= 1
fuzz: native-build
	ZLIB_FUZZ_ITER=$(ITER) ZLIB_FUZZ_MAXSIZE=$(MAXSIZE) ZLIB_FUZZ_SEED=$(SEED) \
		go test . -run TestCrossCheckFuzz -timeout 3600s -v

# Build an ASan/LSan-instrumented gzip_ref against a zlib source tree.
# $(call build-referee-asan,<zlib-src-dir>,<obj-subdir>,<output-binary>)
define build-referee-asan
	mkdir -p bin/$(2)
	for f in $(ZLIB_OBJS); do \
		cc -O1 -g -fsanitize=address,leak -c $(1)/$$f.c -o bin/$(2)/$$f.o -I$(1) || exit 1; \
	done
	c++ -O1 -g -fsanitize=address,leak -std=c++17 native/gzip_ref.cpp bin/$(2)/*.o -I$(1) -o bin/$(3)
endef

# ASan/LSan over every gzip_ref mode (compress/stream/header/bench) for both
# official zlib builds — the whole C surface this repository executes
asan-check: zlib-src zlib-src-132
	$(call build-referee-asan,$(ZLIB131_SRC),zlibobj-asan,gzip_ref_asan)
	./native/asan_sweep.sh ./bin/gzip_ref_asan
	$(call build-referee-asan,$(ZLIB132_SRC),zlibobj-asan-132,gzip_ref_asan_132)
	./native/asan_sweep.sh ./bin/gzip_ref_asan_132

clean:
	go clean ./...
	rm -rf bin .cache
