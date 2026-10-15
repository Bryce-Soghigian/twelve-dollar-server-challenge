#!/usr/bin/env bash
# Builds the server as a normal user. No third-party Go modules.
#
# SQLite is built from the pinned amalgamation with profile-guided optimization:
#   1. compile SQLite with -fprofile-generate
#   2. run sqlitebuild/train.c: the server's exact statements, pragmas and
#      group-commit pattern against a throwaway seed-like database
#   3. recompile SQLite with -fprofile-use and link it statically
# SQLITE_PGO=0 skips 2-3. Go build tag `systemsqlite` links Ubuntu's libsqlite3 instead.
set -euo pipefail
cd "$(dirname "$0")"

SQLITE_VER=3540000
SQLITE_URL="https://sqlite.org/2026/sqlite-autoconf-${SQLITE_VER}.tar.gz"
SQLITE_SHA256=134ec0802dda5795816e25d25872d20b312cb3973438c49b30bc40b7705ea9ed

B=build
mkdir -p "$B"
if [ ! -f "$B/sqlite3.c" ] || ! grep -q "$SQLITE_VER" "$B/.version" 2>/dev/null; then
  curl -fsSL -o "$B/sqlite.tar.gz" "$SQLITE_URL"
  echo "$SQLITE_SHA256  $B/sqlite.tar.gz" | sha256sum -c --quiet
  tar -xzf "$B/sqlite.tar.gz" -C "$B" --strip-components=1 "sqlite-autoconf-${SQLITE_VER}/sqlite3.c" "sqlite-autoconf-${SQLITE_VER}/sqlite3.h"
  rm "$B/sqlite.tar.gz"
  echo "$SQLITE_VER" > "$B/.version"
fi

# SQLite's recommended options for speed, plus what this app doesn't use.
# THREADSAFE=2 (not 0): the checkpoint goroutine uses a second connection on another thread.
DEFS=(
  -DSQLITE_THREADSAFE=2 -DSQLITE_DEFAULT_MEMSTATUS=0 -DSQLITE_DEFAULT_WAL_SYNCHRONOUS=1
  -DSQLITE_DQS=0 -DSQLITE_LIKE_DOESNT_MATCH_BLOBS -DSQLITE_MAX_EXPR_DEPTH=0
  -DSQLITE_OMIT_DECLTYPE -DSQLITE_OMIT_DEPRECATED -DSQLITE_OMIT_PROGRESS_CALLBACK
  -DSQLITE_OMIT_SHARED_CACHE -DSQLITE_OMIT_LOAD_EXTENSION -DSQLITE_USE_ALLOCA
  -DSQLITE_TEMP_STORE=3 -DSQLITE_MAX_MMAP_SIZE=4294967296
)
CFLAGS=(-O2 -march=native -fno-semantic-interposition)

if [ "${SQLITE_PGO:-1}" = 1 ]; then
  rm -f "$B"/*.gcda
  gcc "${CFLAGS[@]}" "${DEFS[@]}" -fprofile-generate -fprofile-update=single -c "$B/sqlite3.c" -o "$B/sqlite3.o"
  gcc "${CFLAGS[@]}" -fprofile-generate -I"$B" sqlitebuild/train.c "$B/sqlite3.o" -o "$B/train" -lm
  "$B/train" "$B/train.db" "${SQLITE_TRAIN_REQUESTS:-150000}"
  rm -f "$B"/train.db*
  # -fprofile-partial-training: code the run never hit stays optimized normally instead of for size.
  gcc "${CFLAGS[@]}" "${DEFS[@]}" -fprofile-use -fprofile-partial-training -fprofile-correction \
    -Wno-missing-profile -c "$B/sqlite3.c" -o "$B/sqlite3.o"
else
  gcc "${CFLAGS[@]}" "${DEFS[@]}" -c "$B/sqlite3.c" -o "$B/sqlite3.o"
fi
rm -f "$B/libsqlite3.a" && ar rcs "$B/libsqlite3.a" "$B/sqlite3.o"

CGO_ENABLED=1 /usr/local/go/bin/go build -tags "${GO_TAGS:-}" -trimpath -ldflags='-s -w' -o bin/server .
