#!/usr/bin/env bash
# ASan/LSan sweep: drives every gzip_ref mode (compress/stream/header/bench)
# with edge-case parameters — long header strings with high bytes, the
# empty-present and 65535-byte Extra boundaries, MTIME/OS extremes, every
# flush type and level, empty input. Combined with an address+leak-sanitized
# gzip_ref build this covers the whole C surface this repository executes.
#
# Usage: asan_sweep.sh <path-to-sanitized-gzip_ref>
set -euo pipefail
REF="$1"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

head -c 300000 /dev/urandom > "$tmp/in.bin"
: > "$tmp/empty.bin"

hex() { od -An -v -tx1 | tr -d ' \n'; }

for level in -1 0 1 6 9; do
  "$REF" compress "$level" 123456789 3           < "$tmp/in.bin" > /dev/null
  "$REF" compress "$level" 4294967295 255 150000 < "$tmp/in.bin" > /dev/null
done
"$REF" compress 6 0 3 < "$tmp/empty.bin" > /dev/null

# stream call sequences: every flush type, zero-length ops, stored level 0
"$REF" stream 6 0@100000 2@100000 4@100000   < "$tmp/in.bin" > /dev/null
"$REF" stream 9 1@50000 3@50000 2@0 4@200000 < "$tmp/in.bin" > /dev/null
"$REF" stream 0 0@150000 4@150000            < "$tmp/in.bin" > /dev/null
"$REF" stream 1 4@300000                     < "$tmp/in.bin" > /dev/null
"$REF" stream 6 4@0                          < "$tmp/empty.bin" > /dev/null

# deflateSetHeader parameter shapes
name_hex="$(head -c 1024 /dev/urandom | tr '\000' '\001' | hex)"
comment_hex="$(head -c 255 /dev/urandom | tr '\000' '\001' | hex)"
extra_max_hex="$(head -c 65535 /dev/urandom | hex)"
for level in -1 0 1 6 9; do
  "$REF" header "$level" 42 3 6e616d65 636f6d6d656e74 dead                    < "$tmp/in.bin" > /dev/null
  "$REF" header "$level" 4294967295 255 "$name_hex" "$comment_hex" "$extra_max_hex" < "$tmp/in.bin" > /dev/null
  "$REF" header "$level" 0 0 - - ""                                           < "$tmp/in.bin" > /dev/null
  "$REF" header "$level" 1 11 - - -                                           < "$tmp/empty.bin" > /dev/null
done

"$REF" bench 6 20 < "$tmp/in.bin" > /dev/null
"$REF" version > /dev/null

echo "asan_sweep: OK"
