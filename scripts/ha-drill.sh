#!/usr/bin/env bash
# Failover drills against the local HA tier (scripts/ha-local.sh) with the
# platform running on it (scripts/dev-local.sh up after
# `eval "$(scripts/ha-local.sh env)"`). Each drill runs SMPP load, crashes
# one component 20 s in, times the failover, brings the node back as a
# replica and then checks money and messages (billing-check).
#
#   scripts/ha-drill.sh postgres          # kill -9 the Patroni leader and its PostgreSQL
#   scripts/ha-drill.sh redis             # kill -9 the Redis primary
#   scripts/ha-drill.sh clickhouse        # kill -9 one ClickHouse replica
#   scripts/ha-drill.sh clickhouse-quorum # stop two of three (Keeper quorum lost) for 30 s
#   scripts/ha-drill.sh all
#
# TPS (default 500) and DURATION (default 90s) set the load.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
D=${HA_DIR:-/var/tmp/amaze-ha}
B=$ROOT/bin
PATRONICTL=${PATRONICTL:-$(dirname "$(command -v "${PATRONI_BIN:-patroni}")")/patronictl}
eval "$("$ROOT/scripts/ha-local.sh" env)"
export TPS=${TPS:-500} DURATION=${DURATION:-90s}
OUT=$ROOT/.dev/drills
mkdir -p "$OUT"

now() { date +%s.%N; }
# within: run the command every 0.5 s until it succeeds, for at most $1 s.
within() {
  local limit=$1; shift
  local end=$(( $(date +%s) + limit ))
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -ge "$end" ] && { echo "  FAILED: not within ${limit} s: $*" >&2; return 1; }
    sleep 0.5
  done
}
leader_changed() { local l; l=$(pg_leader); [ -n "$l" ] && [ "$l" != "$1" ]; }
primary_changed() { local p; p=$(redis_primary); [ -n "$p" ] && [ "$p" != "$1" ]; }
pg_writable() { PGCONNECT_TIMEOUT=2 psql "$1" -Atc "SELECT NOT pg_is_in_recovery()" 2>/dev/null | grep -q t; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.1f", b - a }'; }
pg_leader() { curl -s http://127.0.0.1:8008/cluster | python3 -c 'import json,sys; print(next((m["name"] for m in json.load(sys.stdin)["members"] if m["role"]=="leader"), ""))' 2>/dev/null ||
  for n in 2 3; do curl -s "http://127.0.0.1:$((8007 + n))/cluster" | python3 -c 'import json,sys; print(next((m["name"] for m in json.load(sys.stdin)["members"] if m["role"]=="leader"), ""))' 2>/dev/null && break; done; }
redis_primary() { for p in 26380 26381 26382; do redis-cli -p $p SENTINEL get-master-addr-by-name amaze 2>/dev/null | paste -sd: && return; done; }
ch_count() { curl -s -u "sms:$PGPASSWORD" "http://127.0.0.1:$1/" --data-binary "SELECT count() FROM sms.message_records" 2>/dev/null || echo down; }

load_start() {
  SINCE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  sleep 1
  "$ROOT/scripts/dev-local.sh" load >"$OUT/$1.load.log" 2>&1 &
  LOAD=$!
  sleep 20
}
load_end() { # drill name
  wait "$LOAD" || true
  grep -E "^(submits|submit_sm_resp|receipts|RESULT)" "$OUT/$1.load.log" | sed 's/^/  load: /'
}
check() { # drill name
  "$B/billing-check" -since "$SINCE" >"$OUT/$1.billing.log" 2>&1 || true
  grep -E "PASS|FAIL" "$OUT/$1.billing.log" | sed 's/^/  billing: /'
}

drill_postgres() {
  echo "== PostgreSQL: crash the Patroni leader"
  load_start postgres
  local old; old=$(pg_leader)
  local n=${old#pg}
  local t0; t0=$(now)
  kill -9 "$(cat "$D/pids/patroni$n.pid")"
  kill -9 "$(head -1 "$D/pg$n/data/postmaster.pid")"
  echo "  killed $old (leader) at +20 s"
  within 120 leader_changed "$old"
  echo "  new leader $(pg_leader) after $(since "$t0") s"
  within 60 pg_writable "$POSTGRES_URL"
  echo "  writes accepted (multi-host URL) after $(since "$t0") s"
  within 60 pg_writable "postgres://sms:$PGPASSWORD@127.0.0.1:5000/sms?sslmode=disable"
  echo "  HAProxy :5000 on the new primary after $(since "$t0") s"
  load_end postgres
  nohup setpriv --reuid=postgres --regid=postgres --init-groups -- "${PATRONI_BIN:-patroni}" "$D/pg$n/patroni.yml" >>"$D/logs/patroni$n.log" 2>&1 &
  echo $! >"$D/pids/patroni$n.pid"
  within 180 curl -sf "http://127.0.0.1:$((8007 + n))/replica"
  echo "  $old rejoined as a replica (pg_rewind) after $(since "$t0") s"
  check postgres
}

drill_redis() {
  echo "== Redis: crash the primary"
  load_start redis
  local old; old=$(redis_primary)
  local port=${old##*:}
  local n=$((port - 6380))
  local t0; t0=$(now)
  kill -9 "$(cat "$D/pids/redis$n.pid")"
  echo "  killed primary $old at +20 s"
  within 60 primary_changed "$old"
  echo "  Sentinel promoted $(redis_primary) after $(since "$t0") s"
  load_end redis
  nohup redis-server "$D/redis$n/redis.conf" >>"$D/logs/redis$n.log" 2>&1 &
  echo $! >"$D/pids/redis$n.pid"
  within 60 sh -c "redis-cli -p $port -a $PGPASSWORD --no-auth-warning info replication | grep -q role:slave"
  echo "  old primary rejoined as a replica after $(since "$t0") s"
  # Writes in flight when the primary died may not have reached the replica.
  echo "  wallet-resync -dry-run:"
  "$B/wallet-resync" -dry-run 2>&1 | grep -v "^redis:" | sed 's/^/    /' || true
  "$B/wallet-resync" 2>&1 | grep -v "^redis:" | tail -1 | sed 's/^/  wallet-resync: /'
  check redis
}

drill_clickhouse() {
  echo "== ClickHouse: crash one replica"
  load_start clickhouse
  local t0; t0=$(now)
  kill -9 "$(cat "$D/pids/ch1.pid")"
  echo "  killed ch1 at +20 s"
  sleep 20
  echo "  ch2 rows $(ch_count 8125), ch3 rows $(ch_count 8126) (writer moved on)"
  "$CLICKHOUSE_BIN" server --config-file="$D/ch1/config.xml" >>"$D/logs/ch1.log" 2>&1 &
  echo $! >"$D/pids/ch1.pid"
  within 60 curl -sf http://127.0.0.1:8124/ping
  echo "  ch1 back after $(since "$t0") s"
  load_end clickhouse
  check clickhouse
  sleep 10
  echo "  rows: ch1 $(ch_count 8124), ch2 $(ch_count 8125), ch3 $(ch_count 8126)"
}

drill_clickhouse_quorum() {
  echo "== ClickHouse: lose the Keeper quorum (two of three replicas down for 30 s)"
  load_start clickhouse-quorum
  local t0; t0=$(now)
  kill "$(cat "$D/pids/ch2.pid")" "$(cat "$D/pids/ch3.pid")"
  echo "  stopped ch2 and ch3 at +20 s; ch1 is read-only, inserts wait in Kafka"
  sleep 30
  for n in 2 3; do
    "$CLICKHOUSE_BIN" server --config-file="$D/ch$n/config.xml" >>"$D/logs/ch$n.log" 2>&1 &
    echo $! >"$D/pids/ch$n.pid"
  done
  within 120 sh -c '[ "$(curl -s http://127.0.0.1:8124/ --data-binary "SELECT countIf(is_readonly) FROM system.replicas")" = 0 ]' 
  echo "  quorum back, ch1 writable after $(since "$t0") s"
  load_end clickhouse-quorum
  check clickhouse-quorum
  sleep 10
  echo "  rows: ch1 $(ch_count 8124), ch2 $(ch_count 8125), ch3 $(ch_count 8126)"
}

case "${1:-}" in
postgres) drill_postgres ;;
redis) drill_redis ;;
clickhouse) drill_clickhouse ;;
clickhouse-quorum) drill_clickhouse_quorum ;;
all) drill_postgres; drill_redis; drill_clickhouse; drill_clickhouse_quorum ;;
*) echo "usage: $0 postgres|redis|clickhouse|clickhouse-quorum|all" >&2; exit 2 ;;
esac
