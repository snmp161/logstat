#!/usr/bin/env bash
# Soak a release candidate against a real log file, a real Redis and a real
# clock. The test suite runs in seconds and fakes the clock; this is the run
# that catches what only shows up over time — a reset that never fires, a TTL
# that lapses, memory that creeps, a rotation that loses the tail.
#
# Nothing here needs root or redis-cli: the daemon runs in the foreground as the
# current user, everything lives under a temporary directory, the numbers are
# read back through the exporter, and the keys are written with a one-minute TTL
# so Redis cleans up after itself once the run is over.
#
#   ./scripts/soak.sh                       # 10 minutes, reset every minute
#   DURATION=24h RESET='0 * * * *' ./scripts/soak.sh
#
# Requires: a Redis reachable at REDIS_HOST:REDIS_PORT and curl.
set -euo pipefail

DURATION=${DURATION:-10m}
RESET=${RESET:-* * * * *}   # every minute, so a short soak still sees resets
RATE=${RATE:-20}            # log lines per second
REDIS_HOST=${REDIS_HOST:-127.0.0.1}
REDIS_PORT=${REDIS_PORT:-6379}
REDIS_DB=${REDIS_DB:-15}
METRICS_PORT=${METRICS_PORT:-19850}
ROTATE_EVERY=${ROTATE_EVERY:-120}   # seconds between simulated logrotate runs
SAMPLE_EVERY=${SAMPLE_EVERY:-30}    # seconds between report lines

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d /tmp/logstat-soak.XXXXXX)
log=$work/app.log

seconds() { # 90s / 10m / 24h -> seconds
    case "$1" in
        *h) echo $(( ${1%h} * 3600 )) ;;
        *m) echo $(( ${1%m} * 60 )) ;;
        *s) echo "${1%s}" ;;
        *)  echo "$1" ;;
    esac
}
total=$(seconds "$DURATION")

cleanup() {
    [[ -n ${writer_pid:-} ]] && kill "$writer_pid" 2>/dev/null
    [[ -n ${daemon_pid:-} ]] && kill "$daemon_pid" 2>/dev/null
    wait 2>/dev/null || true
    echo "workdir kept at $work (Redis keys expire on their own within a minute)"
}
trap cleanup EXIT

command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }

cat >"$work/config.yaml" <<EOF
log_path: $log
actions: [get-number, get-sms, getNumber, getStatus]
case_sensitive: yes
flush_interval: 5
lock_file: $work/logstat.lock
redis:
  host: $REDIS_HOST
  port: $REDIS_PORT
  db: $REDIS_DB
  ttl: 60          # short on purpose: a lapsing TTL shows up within the soak
logging:
  level: info
  output: file
  file: $work/daemon.log
reset:
  enabled: true
  schedule: "$RESET"
metrics:
  enabled: true
  listen: 127.0.0.1:$METRICS_PORT
  path: /metrics
EOF

echo "==> building"
make -C "$root" build >/dev/null
: >"$log"

echo "==> soaking for $DURATION (reset '$RESET', ${RATE} lines/s, rotation every ${ROTATE_EVERY}s)"
echo "    workdir  $work"
echo "    metrics  http://127.0.0.1:$METRICS_PORT/"

"$root/dist/logstat" run --config "$work/config.yaml" &
daemon_pid=$!

# The writer appends and then rotates the file the way logrotate would,
# alternating between the two schemes the daemon has to survive.
(
    tick=0
    rotations=0
    while true; do
        for _ in $(seq "$RATE"); do
            printf '%s "GET /api/get-number" 200\n%s "GET /api/get-sms" 200\n' \
                "$(date -Iseconds)" "$(date -Iseconds)" >>"$log"
        done
        sleep 1
        tick=$((tick + 1))
        if (( tick % ROTATE_EVERY == 0 )); then
            rotations=$((rotations + 1))
            if (( rotations % 2 )); then
                mv "$log" "$log.1" && : >"$log"          # logrotate create
            else
                cp "$log" "$log.1" && : >"$log"          # logrotate copytruncate
            fi
        fi
    done
) &
writer_pid=$!

metric() { awk -v k="$1" '$1 == k {print $2}' <<<"$2"; }

started=$(date +%s)
printf '%-8s %-8s %-11s %-10s %-9s %-8s %s\n' \
    elapsed rss_kb lines_read in_memory in_redis pending resets
while (( $(date +%s) - started < total )); do
    sleep "$SAMPLE_EVERY"
    metrics=$(curl -sf "http://127.0.0.1:$METRICS_PORT/metrics" || echo "")
    printf '%-8s %-8s %-11s %-10s %-9s %-8s %s\n' \
        "$(( $(date +%s) - started ))s" \
        "$(awk '/VmRSS/ {print $2}' "/proc/$daemon_pid/status" 2>/dev/null || echo -)" \
        "$(metric logstat_lines_read_total "$metrics")" \
        "$(metric 'logstat_matched_lines_total{action="get-number"}' "$metrics")" \
        "$(metric 'logstat_redis_counter{action="get-number"}' "$metrics")" \
        "$(metric 'logstat_pending_increments{action="get-number"}' "$metrics")" \
        "$(grep -c "counters reset" "$work/daemon.log" 2>/dev/null || echo 0)"
done

echo
echo "==> daemon log: warnings and errors"
grep -E "WARN|ERROR" "$work/daemon.log" || echo "    (none)"
echo
echo "==> what to check"
echo "    * rss_kb flat, not climbing"
echo "    * lines_read and in_memory growing, pending back near 0 after each flush"
echo "    * resets > 0, and in_redis dropping to 0 on schedule while in_memory keeps rising"
echo "    * no WARN/ERROR beyond the ones you caused"
