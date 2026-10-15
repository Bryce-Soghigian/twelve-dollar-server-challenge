#!/usr/bin/env bash
# CPU profile (perf, incl. kernel + SQLite C frames) of the server under one wrk workload.
#   bash local/profile.sh [feed|posts|writes] [conns]
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"; ROOT="$(cd "$HERE/../../.." && pwd)"
T="${1:-feed}"; CONNS="${2:-256}"
docker build -q -t twelve-perf -f "$HERE/Dockerfile.perf" "$HERE" >/dev/null
docker rm -f twelve-srv >/dev/null 2>&1 || true
docker run -d --name twelve-srv --network twelve --privileged --cpus=1 --memory=2g --ulimit nofile=65535:65535 \
  -e HOST=0.0.0.0 -e PORT=3000 -e SQLITE_PATH=/data/feed.db -v "$ROOT/seed/feed.db":/seed/feed.db:ro twelve-perf \
  bash -c 'mkdir -p /data && cp /seed/feed.db /data/ && /usr/local/go/bin/go build -o bin/server . && exec bash start.sh' >/dev/null
until docker run --rm --network twelve twelve-wrk bash -c 'exec 3<>/dev/tcp/twelve-srv/3000' 2>/dev/null; do sleep 0.5; done
case $T in posts) S="-s /s/posts.lua http://twelve-srv:3000";; writes) S="-s /s/writes.lua http://twelve-srv:3000";; *) S=http://twelve-srv:3000/feed;; esac
docker run -d --rm --ulimit nofile=65535:65535 --network twelve -v "$HERE":/s twelve-wrk wrk -t4 -c$CONNS -d20s $S >/dev/null
sleep 4
docker exec twelve-srv bash -c 'perf record -q -F 1999 -g -p $(pidof server) -o /tmp/p.data -- sleep 10 >/dev/null 2>&1
  echo "== by component (self time)"; perf report -i /tmp/p.data --no-children --sort dso -g none -q 2>/dev/null | grep -v "^$" | head -6
  echo "== top symbols (self time)";  perf report -i /tmp/p.data --no-children --sort sym -g none -q 2>/dev/null | grep -v "^$" | head -30'
docker rm -f twelve-srv >/dev/null
