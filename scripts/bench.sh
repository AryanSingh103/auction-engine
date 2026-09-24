#!/usr/bin/env bash
# Runs the M2 benchmark matrix inside docker compose and stores every raw
# result. See docs/benchmarks.md for how to read the output.
#
#   scripts/bench.sh <output-dir> [duration-seconds] [warmup-seconds] [reps]
#
# Every run starts from a fresh database (new volume, migrated from scratch)
# so later runs are not slowed by tables and indexes grown by earlier ones,
# and the run order is interleaved (rep -> config -> strategy) so slow drift
# (thermal throttling, background apps) cannot favor one strategy.
set -euo pipefail

out=${1:?usage: scripts/bench.sh <output-dir> [duration-seconds] [warmup-seconds] [reps]}
duration=${2:-30}
warmup=${3:-5}
reps=${4:-3}
# Server-side snapshot window: the whole run (warmup + measurement + the
# post-run checks and final scrape), but short enough never to reach back
# into the previous run, which ended before this run's fresh_stack.
window="$(( warmup + duration + 8 ))s"
mkdir -p "$out"

# name workers auctions
configs=(
  "hot-w2 2 1"
  "hot-w10 10 1"
  "hot-w50 50 1"
  "hot-w200 200 1"
  "spread-w10 10 5"
  "spread-w50 50 25"
  "spread-w200 200 100"
)
strategies=(pessimistic optimistic)

fresh_stack() { # $1 = BID_LOCKING
  docker compose stop api >/dev/null 2>&1 || true
  docker compose rm -sf postgres migrate >/dev/null 2>&1
  docker volume rm -f auction-engine_pgdata >/dev/null
  BID_LOCKING=$1 docker compose up -d --wait postgres migrate api prometheus >/dev/null 2>&1
}

prom() { # $1 = PromQL, evaluated at the end of the run over its window
  curl -s --data-urlencode "query=$1" "http://127.0.0.1:${PROMETHEUS_HOST_PORT:-9090}/api/v1/query" |
    python3 -c 'import json,sys; r=json.load(sys.stdin)["data"]["result"]; print(r[0]["value"][1] if r else "null")'
}

run() { # $1 label, $2 strategy, $3.. loadgen flags
  local label=$1 strategy=$2; shift 2
  local file="$out/$label.json"
  echo "== $label ($strategy) $(date -u +%H:%M:%S)"
  fresh_stack "$strategy"
  # A failed verification check exits non-zero: keep the report (it records
  # which check failed) but mark the run as unusable.
  if ! docker compose --profile bench run --rm -T loadgen \
      --label="$label" --warmup="${warmup}s" --duration="${duration}s" --out=- "$@" > "$file" 2> "$out/$label.log"; then
    echo "   CHECKS FAILED: see $out/$label.log"
    mv "$file" "$out/$label.FAILED.json"
    return
  fi
  sleep 6 # let Prometheus take a final scrape
  local w=$window
  cat > "$out/$label.server.json" <<JSON
{
  "lock_wait_p50_s": $(prom "histogram_quantile(0.5, sum by (le) (increase(bid_lock_wait_seconds_bucket[$w])))"),
  "lock_wait_p99_s": $(prom "histogram_quantile(0.99, sum by (le) (increase(bid_lock_wait_seconds_bucket[$w])))"),
  "tx_p99_s": $(prom "histogram_quantile(0.99, sum by (le) (increase(bid_transaction_seconds_bucket[$w])))"),
  "optimistic_conflicts": $(prom "sum(increase(bid_optimistic_conflicts_total[$w]))"),
  "contention_failures": $(prom "sum(increase(bids_total{outcome=\"contention\"}[$w]))"),
  "pool_empty_acquires": $(prom "sum(increase(db_pool_empty_acquires_total[$w]))"),
  "api_cpu_seconds": $(prom "sum(increase(process_cpu_seconds_total{job=\"api\"}[$w]))")
}
JSON
  grep -E '^(throughput|latency|results)' "$out/$label.log" | sed 's/^/   /'
}

# Build every image from the current source first. `docker compose run`
# and `up` do not rebuild on their own, so without this a benchmark could
# silently run stale code. The image IDs recorded below are what ran.
docker compose --profile bench build --quiet api migrate loadgen

# Environment, for traceability.
{
  echo "date_utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "git_commit: $(git rev-parse HEAD)"
  echo "host: $(sysctl -n machdep.cpu.brand_string) / $(sysctl -n hw.ncpu) cores / $(( $(sysctl -n hw.memsize) / 1073741824 )) GiB / macOS $(sw_vers -productVersion)"
  echo "colima: $(colima list | tail -n 1 | tr -s ' ')"
  echo "go: $(go version)"
  echo "docker_server: $(docker version --format '{{.Server.Version}}')"
  echo "images:"
  for img in postgres:18.6-alpine3.24 prom/prometheus:v3.14.0 auction-engine-api auction-engine-migrate auction-engine-loadgen; do
    echo "  $img $(docker image inspect -f '{{.Id}}' "$img" | cut -c1-19)"
  done
  echo "config: duration=$duration warmup=$warmup reps=$reps DB_MAX_CONNS=$(grep ^DB_MAX_CONNS .env | cut -d= -f2)"
} > "$out/environment.txt"

for rep in $(seq 1 "$reps"); do
  run "control-healthz-w50-r$rep" pessimistic --mode=healthz --workers=50
  for cfg in "${configs[@]}"; do
    read -r name workers auctions <<< "$cfg"
    for s in "${strategies[@]}"; do
      run "$s-$name-r$rep" "$s" --workers="$workers" --auctions="$auctions"
    done
  done
done

# Leave the default (pessimistic) stack running.
fresh_stack pessimistic
echo "done: $out"
