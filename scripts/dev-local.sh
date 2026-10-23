#!/usr/bin/env bash
# Runs the whole platform locally without Docker (Linux / WSL / macOS):
# in-memory Kafka (devkafka), the SMPP vendor simulator and all services,
# against a local PostgreSQL and Redis.
#
#   scripts/dev-local.sh up      # build, reset dev data, start everything
#   scripts/dev-local.sh down    # stop everything
#   scripts/dev-local.sh load    # 30 s load test at $TPS (default 1000)
#
# Needs: Go, a PostgreSQL database `sms` owned by user sms/sms, Redis on 6379.
# Optional: CHDB_PYTHON=<python with `pip install chdb`> runs a ClickHouse
# stand-in on :8123 and the ch-writer; without it, ch-writer is skipped.
# CLICKHOUSE_URL=<url[,url...]> uses a real ClickHouse instead (e.g. the HA
# tier from scripts/ha-local.sh: `eval "$(scripts/ha-local.sh env)"` first,
# which also points PostgreSQL and Redis at it).
# ROUTE=httpsim sends the demo customer's traffic through the HTTP vendor.
# WARNING: `up` wipes the `sms` database schema and FLUSHALLs Redis.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
B=$ROOT/bin
L=$ROOT/.dev/logs

export APP_ENV=dev LOG_LEVEL=${LOG_LEVEL:-info}
export POSTGRES_URL=${POSTGRES_URL:-postgres://sms:sms@localhost:5432/sms?sslmode=disable}
export REDIS_ADDRS=${REDIS_ADDRS:-localhost:6379}
export KAFKA_BROKERS=${KAFKA_BROKERS:-localhost:9092}

P=$ROOT/.dev/pids

# Processes are tracked by PID file, so stopping never matches unrelated
# command lines (pattern-based pkill can hit the calling shell).
stop() {
  mkdir -p "$P"
  local f pid
  for f in "$P"/*.pid; do
    [ -e "$f" ] || continue
    pid=$(cat "$f")
    kill "$pid" 2>/dev/null || true
  done
  for _ in $(seq 1 40); do
    local alive=0
    for f in "$P"/*.pid; do
      [ -e "$f" ] || continue
      kill -0 "$(cat "$f")" 2>/dev/null && alive=1
    done
    [ "$alive" = 0 ] && { rm -f "$P"/*.pid; return 0; }
    sleep 0.25
  done
  echo "some processes did not stop:" >&2
  for f in "$P"/*.pid; do kill -0 "$(cat "$f")" 2>/dev/null && echo "  $(basename "$f" .pid)" >&2; done
  return 1
}

start() {
  local name=$1; shift
  mkdir -p "$P"
  nohup env "$@" >"$L/$name.log" 2>&1 &
  echo $! >"$P/$name.pid"
}

case "${1:-up}" in
up)
  mkdir -p "$L"
  (cd "$ROOT" && go build -o "$B/" ./cmd/...)
  stop
  rm -f "$L"/*.log
  redis_primary=${REDIS_ADDRS%%,*}
  if [ -n "${REDIS_SENTINEL_MASTER:-}" ]; then
    redis_primary=$(REDISCLI_AUTH=${REDIS_SENTINEL_PASSWORD:-} redis-cli -u "redis://$redis_primary" SENTINEL get-master-addr-by-name "$REDIS_SENTINEL_MASTER" | paste -sd:)
  fi
  REDISCLI_AUTH=${REDIS_PASSWORD:-} redis-cli -u "redis://$redis_primary" FLUSHALL >/dev/null
  psql "$POSTGRES_URL" -q -o /dev/null -c "SET client_min_messages=warning" -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
  "$B/migrate" >"$L/migrate.log" 2>&1
  start devkafka "$B/devkafka" -port 9092 -partitions 12
  start smpp-sim "$B/smpp-sim" -addr :2776 -dlr-delay "${DLR_DELAY:-200ms}" -fail "${FAIL:-0.02}"
  start http-sim "$B/http-sim" -addr :2777 -dlr-url "http://127.0.0.1:8090/dlr/httpsim?token=devtoken" \
    -dlr-delay "${DLR_DELAY:-200ms}" -fail "${FAIL:-0.02}"
  CH_URL=${CLICKHOUSE_URL:-}
  if [ -n "${CHDB_PYTHON:-}" ]; then
    CH_URL=http://localhost:8123
    rm -rf "$ROOT/.dev/chdb"
    start ch-dev-server "$CHDB_PYTHON" -I "$ROOT/scripts/ch-dev-server.py" --port 8123 --path "$ROOT/.dev/chdb"
    for _ in $(seq 1 60); do curl -sf localhost:8123/ping >/dev/null && break; sleep 0.5; done
  fi
  sleep 1
  "$B/devseed" -route "${ROUTE:-sim}" ${SEED_ARGS:-} | tee "$L/seed.log"
  start smpp-server   OPS_ADDR=:9101 "$B/smpp-server"
  start http-api      OPS_ADDR=:9102 "$B/http-api"
  if [ -n "${PROMETHEUS_BIN:-}" ]; then
    mkdir -p "$ROOT/.dev/prom"
    printf 'global: {scrape_interval: 5s}\nscrape_configs:\n  - job_name: platform\n    static_configs: [{targets: [%s]}]\n' \
      "$(printf '"localhost:%s",' 9101 9102 9103 9104 9105 9106 9107 9109 9110 9111 | sed 's/,$//')" > "$ROOT/.dev/prom/prometheus.yml"
    start prometheus "$PROMETHEUS_BIN" --config.file="$ROOT/.dev/prom/prometheus.yml" --storage.tsdb.path="$ROOT/.dev/prom/data" \
      --web.listen-address=:9090 --log.level=warn
  fi
  [ -d "$ROOT/portal/out" ] || echo "portal not built (cd portal && npm ci && npm run build); admin-api serves the API only"
  start admin-api     OPS_ADDR=:9103 CLICKHOUSE_URL="$CH_URL" \
    PORTAL_DIR="$( [ -d "$ROOT/portal/out" ] && echo "$ROOT/portal/out")" PROMETHEUS_URL="${PROMETHEUS_BIN:+http://localhost:9090}" "$B/admin-api"
  start alerter       OPS_ADDR=:9111 ALERT_EVERY=10s WEBHOOK_ALLOW_PRIVATE=true "$B/alerter"
  start router        OPS_ADDR=:9104 "$B/router"
  start rater         OPS_ADDR=:9105 "$B/rater"
  start dlr-processor OPS_ADDR=:9106 DLR_TIMEOUT="${DLR_TIMEOUT:-72h}" "$B/dlr-processor"
  # One connector runs every vendor (sim and httpsim from devseed, and any
  # added in the portal within VENDOR_RESCAN).
  start connector     OPS_ADDR=:9107 VENDOR_ID='*' VENDOR_RESCAN=5s DLR_ADDR=:8090 "$B/connector"
  start webhook-dispatcher OPS_ADDR=:9109 WEBHOOK_ALLOW_PRIVATE=true "$B/webhook-dispatcher"
  ports="9101 9102 9103 9104 9105 9106 9107 9109 9111"
  if [ -n "$CH_URL" ]; then
    start ch-writer OPS_ADDR=:9110 CLICKHOUSE_URL="$CH_URL" "$B/ch-writer"
    ports="$ports 9110"
  fi
  sleep 4
  for p in $ports; do
    printf 'ops :%s readyz %s\n' "$p" "$(curl -s -o /dev/null -w '%{http_code}' "localhost:$p/readyz")"
  done
  echo "logs: $L"
  ;;
down)
  stop
  ;;
load)
  "$B/smpp-load" -tps "${TPS:-1000}" -duration "${DURATION:-30s}" -binds "${BINDS:-8}" -drain 60s
  ;;
*)
  echo "usage: $0 up|down|load" >&2
  exit 2
  ;;
esac
