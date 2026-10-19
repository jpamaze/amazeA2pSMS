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
# WARNING: `up` wipes the `sms` database schema and FLUSHALLs Redis.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
B=$ROOT/bin
L=$ROOT/.dev/logs

export APP_ENV=dev LOG_LEVEL=${LOG_LEVEL:-info}
export POSTGRES_URL=${POSTGRES_URL:-postgres://sms:sms@localhost:5432/sms?sslmode=disable}
export REDIS_ADDRS=${REDIS_ADDRS:-localhost:6379}
export KAFKA_BROKERS=${KAFKA_BROKERS:-localhost:9092}

stop() {
  pkill -f "$B/" 2>/dev/null || true
  for _ in $(seq 1 40); do pgrep -f "$B/" >/dev/null || return 0; sleep 0.25; done
  echo "some processes did not stop:" >&2
  pgrep -af "$B/" >&2
  return 1
}

start() {
  local name=$1; shift
  nohup env "$@" >"$L/$name.log" 2>&1 &
}

case "${1:-up}" in
up)
  mkdir -p "$L"
  (cd "$ROOT" && go build -o "$B/" ./cmd/...)
  stop
  rm -f "$L"/*.log
  redis-cli -u "redis://${REDIS_ADDRS%%,*}" FLUSHALL >/dev/null
  psql "$POSTGRES_URL" -q -o /dev/null -c "SET client_min_messages=warning" -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
  "$B/migrate" >"$L/migrate.log" 2>&1
  start devkafka "$B/devkafka" -port 9092 -partitions 12
  start smpp-sim "$B/smpp-sim" -addr :2776 -dlr-delay "${DLR_DELAY:-200ms}" -fail "${FAIL:-0.02}"
  sleep 1
  "$B/devseed" ${SEED_ARGS:-} | tee "$L/seed.log"
  start smpp-server   OPS_ADDR=:9101 "$B/smpp-server"
  start http-api      OPS_ADDR=:9102 "$B/http-api"
  start admin-api     OPS_ADDR=:9103 "$B/admin-api"
  start router        OPS_ADDR=:9104 "$B/router"
  start rater         OPS_ADDR=:9105 "$B/rater"
  start dlr-processor OPS_ADDR=:9106 DLR_TIMEOUT="${DLR_TIMEOUT:-72h}" "$B/dlr-processor"
  start connector-sim OPS_ADDR=:9107 VENDOR_ID=sim "$B/connector"
  sleep 4
  for p in 9101 9102 9103 9104 9105 9106 9107; do
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
