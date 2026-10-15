#!/usr/bin/env bash
# Where does the container's memory go? Runs a load, then breaks down cgroup memory.
#   SRV_ENV="-e SQLITE_CACHE_KIB=0" bash local/memprobe.sh [writes|posts|feed] [conns] [secs]
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"; ROOT="$(cd "$HERE/../../.." && pwd)"
T="${1:-writes}"; CONNS="${2:-256}"; SECS="${3:-15}"
docker rm -f twelve-srv >/dev/null 2>&1 || true
# shellcheck disable=SC2086
docker run -d --name twelve-srv --network twelve --security-opt seccomp=unconfined \
  --cpus=1 --memory=2g --ulimit nofile=65535:65535 ${SRV_ENV:-} -e HOST=0.0.0.0 -e PORT=3000 \
  -e SQLITE_PATH=/data/feed.db -v "$ROOT/seed/feed.db":/seed/feed.db:ro twelve-go-uring \
  bash -c 'mkdir -p /data && cp /seed/feed.db /data/ && sync && echo 1 > /proc/sys/vm/drop_caches 2>/dev/null; exec bash start.sh' >/dev/null
until docker run --rm --network twelve twelve-wrk bash -c 'exec 3<>/dev/tcp/twelve-srv/3000' 2>/dev/null; do sleep 0.5; done
snap() {
  docker exec twelve-srv bash -c '
    p=$(pidof server); grep -E "VmRSS|RssAnon|RssFile|RssShmem" /proc/$p/status | tr -s " \t" " " | paste -sd" "
    awk "/^(anon|file|shmem|file_dirty|sock) /{printf \"%s=%dMiB \", \$1, \$2/1048576}" /sys/fs/cgroup/memory.stat; echo
    ls -la /data | awk "NR>1 && \$5>0 {printf \"%s=%dMiB \", \$9, \$5/1048576}"; echo'
}
echo "-- idle"; snap
case $T in posts) S="-s /s/posts.lua http://twelve-srv:3000";; writes) S="-s /s/writes.lua http://twelve-srv:3000";; *) S=http://twelve-srv:3000/feed;; esac
docker run --rm --ulimit nofile=65535:65535 --network twelve -v "$HERE":/s twelve-wrk wrk -t4 -c$CONNS -d${SECS}s $S | grep Requests/sec
echo "-- after $T c=$CONNS ${SRV_ENV:-}"; snap
docker rm -f twelve-srv >/dev/null
