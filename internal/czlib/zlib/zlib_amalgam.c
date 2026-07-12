/*
 * zlib 1.3.1 compression-side amalgamated compilation unit
 *
 * cgo only compiles sources #included from the preamble, so the zlib
 * source files needed by deflate are merged into one compilation unit,
 * removing any link dependency on the system -lz.
 *
 * Contains only compression (deflate) code; for decompression use Go's
 * standard library compress/gzip.
 *
 * Sources come from https://github.com/madler/zlib (v1.3.1), completely
 * unmodified; see LICENSE in this directory (zlib License).
 */

#include "zutil.c"
/* gzguts.h (pulled in via zutil.c) defines GZIP as 2, and deflate.h
   redefines it as empty; the two are unrelated — undef here to silence
   the redefinition warning */
#undef GZIP
#include "adler32.c"
#include "crc32.c"
#include "deflate.c"
#include "trees.c"
