#!/usr/bin/env bash
# The challenge's own load test (bench/load.js, unmodified) against the server on 1 CPU / 2 GB.
#   bash local/k6.sh <VUS> [DURATION=5m] [RAMP_UP=60s]
# Prints k6's summary plus server CPU/memory and k6 memory sampled every 10 s.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"; ROOT="$(cd "$HERE/../../.." && pwd)"
VUS="$1"; DURATION="${2:-5m}"; RAMP="${3:-60s}"
# Keep the Mac awake for the run: idle sleep pauses the VM and shows up as minutes-long "requests".
command -v caffeinate >/dev/null && { caffeinate -is -w $$ & }
if command -v pmset >/dev/null && pmset -g batt | grep -q "Battery Power"; then
  echo "WARNING: on battery ($(pmset -g batt | grep -oE '[0-9]+%')) - macOS throttles CPU; results are not comparable" >&2
fi
docker network inspect twelve >/dev/null 2>&1 || docker network create twelve >/dev/null
docker rm -f twelve-srv twelve-k6 >/dev/null 2>&1 || true
# shellcheck disable=SC2086
docker run -d --name twelve-srv --network twelve --security-opt seccomp=unconfined \
  --cpus=1 --memory=2g --memory-swap=2g --ulimit nofile=65535:65535 ${SRV_ENV:-} \
  -e HOST=0.0.0.0 -e PORT=80 -e JWT_SECRET=twelve-dollar-challenge -e SQLITE_PATH=/data/feed.db \
  -v "$ROOT/seed/feed.db":/seed/feed.db:ro "${IMAGE:-twelve-go-uring}" \
  bash -c 'mkdir -p /data && cp /seed/feed.db /data/ && exec bash start.sh' >/dev/null
until docker run --rm --network twelve twelve-wrk bash -c 'exec 3<>/dev/tcp/twelve-srv/80' 2>/dev/null; do sleep 0.5; done
docker run -d --name twelve-k6 --network twelve --ulimit nofile=1048576:1048576 --sysctl net.ipv4.ip_local_port_range="1024 65535" -v "$ROOT":/repo:ro \
  ${K6_OUT:+-v $K6_OUT:/out} grafana/k6:1.3.0 run --quiet --summary-mode=full ${K6_OUT:+--out csv=/out/k6.csv.gz} -e BASE_URL=http://twelve-srv -e VUS="$VUS" -e DURATION="$DURATION" -e RAMP_UP="$RAMP" \
  /repo/bench/load.js >/dev/null
# TcpExt counters that show dropped/retried connection attempts.
TCPX='awk "/^TcpExt:/{if(!h){split(\$0,k);h=1}else{split(\$0,v);for(i=2;i<=NF;i++)if(k[i]~/^(ListenOverflows|ListenDrops|TCPSynRetrans|TCPTimeouts|TCPAbortOnData|TCPBacklogDrop)$/)printf \"%s=%s \",k[i],v[i]}}" /proc/net/netstat'
K6TCP=""
echo "== VUS=$VUS duration=$DURATION ramp=$RAMP ${SRV_ENV:-}"
while [ "$(docker inspect -f '{{.State.Running}}' twelve-k6)" = true ]; do
  docker stats --no-stream --format '{{.Name}} cpu={{.CPUPerc}} mem={{.MemUsage}}' twelve-srv twelve-k6 | paste -sd' ' -
  K6TCP="$(docker run --rm --network container:twelve-k6 twelve-wrk bash -c "$TCPX" 2>/dev/null || echo "$K6TCP")"
  sleep 10
done
docker logs twelve-k6 2>&1 | grep -vE '^\s*$' | sed -n '/THRESHOLDS/,$p'
echo "k6 exit code: $(docker inspect -f '{{.State.ExitCode}}' twelve-k6)"
echo "server tcp: $(docker exec twelve-srv bash -c "$TCPX")"
echo "k6 tcp (last sample): $K6TCP"
echo "server cgroup cpu.stat: $(docker exec twelve-srv cat /sys/fs/cgroup/cpu.stat | grep -E 'nr_periods|nr_throttled|throttled_usec' | paste -sd' ' -)"
echo "server stalls (>50ms loop passes): $(docker logs twelve-srv 2>&1 | grep -c '^.* stall:')"
docker logs twelve-srv 2>&1 | grep -E "stall:|checkpoint:" | tail -40 || true
docker rm -f twelve-srv twelve-k6 >/dev/null
