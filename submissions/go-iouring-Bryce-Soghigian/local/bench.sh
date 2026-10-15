#!/usr/bin/env bash
# Throughput on a 1 CPU / 2 GB container:
#   bash local/bench.sh [uring|std] [conns] [secs] [tests: "feed posts writes"]
# Extra server env goes in SRV_ENV, e.g. SRV_ENV="-e WRITE_BATCH_MAX=1 -e SQLITE_CACHE_KIB=0".
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"; ROOT="$(cd "$HERE/../../.." && pwd)"
MODE="${1:-uring}"; CONNS="${2:-256}"; SECS="${3:-15}"; TESTS="${4:-feed posts writes}"
docker network inspect twelve >/dev/null 2>&1 || docker network create twelve >/dev/null
docker rm -f twelve-srv >/dev/null 2>&1 || true
# shellcheck disable=SC2086
docker run -d --name twelve-srv --network twelve --security-opt seccomp=unconfined \
  --cpus=1 --memory=2g --ulimit nofile=65535:65535 ${SRV_ENV:-} \
  -e SERVER_MODE="$MODE" -e HOST=0.0.0.0 -e PORT=3000 -e JWT_SECRET=twelve-dollar-challenge \
  -e SQLITE_PATH=/data/feed.db -v "$ROOT/seed/feed.db":/seed/feed.db:ro "${IMAGE:-twelve-go-uring}" \
  bash -c 'mkdir -p /data && cp /seed/feed.db /data/ && exec bash start.sh' >/dev/null
until docker run --rm --network twelve twelve-wrk bash -c 'exec 3<>/dev/tcp/twelve-srv/3000' 2>/dev/null; do sleep 0.5; done
W="docker run --rm --ulimit nofile=65535:65535 --network twelve -v $HERE:/s twelve-wrk wrk -t4 -c$CONNS -d${SECS}s --latency"
$W http://twelve-srv:3000/feed >/dev/null 2>&1  # warm the page cache
for t in $TESTS; do
  echo "== $MODE $t c=$CONNS ${SRV_ENV:-}"
  case $t in
    feed)   $W http://twelve-srv:3000/feed ;;
    posts)  $W -s /s/posts.lua http://twelve-srv:3000 ;;
    writes) $W -s /s/writes.lua http://twelve-srv:3000 ;;
  esac | grep -E 'Requests/sec|99%|Non-2xx|Socket errors' | tr -s ' '
done
docker stats --no-stream --format 'mem={{.MemUsage}}' twelve-srv
docker logs twelve-srv 2>&1 | grep -viE "mode=|loop up" | tail -3 || true
docker rm -f twelve-srv >/dev/null
