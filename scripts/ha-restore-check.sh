#!/usr/bin/env bash
# Backs up the local HA tier (scripts/ha-local.sh) and proves each backup
# restores, the way docs/phase5-ha.md tells operators to:
#   PostgreSQL  full backup to both repositories, then a point-in-time
#               restore from each (S3 and the NFS stand-in) into a scratch
#               instance: a row written before the target time is there,
#               one written after it is not, and the ledger matches
#   ClickHouse  BACKUP to S3 and to the backup disk, RESTORE each as a new
#               table, row counts compared
#   Redis       snapshot from a replica, loaded into a scratch Redis, key
#               counts compared
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
D=${HA_DIR:-/var/tmp/amaze-ha}
PG_BIN=${PG_BIN:-/usr/lib/postgresql/16/bin}
eval "$("$ROOT/scripts/ha-local.sh" env)"
FAIL=0
ok() { echo "  PASS  $*"; }
bad() { echo "  FAIL  $*"; FAIL=1; }

leader_port() {
  for n in 1 2 3; do curl -sf -o /dev/null "http://127.0.0.1:$((8007 + n))/primary" && { echo $((5440 + n)); return; }; done
}
leader_n() { echo $(( $(leader_port) - 5440 )); }

echo "== PostgreSQL (pgBackRest)"
N=$(leader_n)
PGBR="setpriv --reuid=postgres --regid=postgres --init-groups -- pgbackrest --config=$D/pg$N/pgbackrest.conf --stanza=amaze"
P="psql -h 127.0.0.1 -p $(leader_port) -U postgres -v ON_ERROR_STOP=1 -Atq"
$P -c "CREATE DATABASE restore_check" 2>/dev/null || true
psql -h 127.0.0.1 -p "$(leader_port)" -U postgres -Atq -d restore_check -c "SET client_min_messages = warning" -c "CREATE TABLE IF NOT EXISTS marks (name text, at timestamptz DEFAULT now())" -c "TRUNCATE marks"
for repo in 1 2; do
  $PGBR --type=full --repo=$repo backup >/dev/null
done
ok "full backup to repo1 (S3) and repo2 (NFS)"
psql -h 127.0.0.1 -p "$(leader_port)" -U postgres -Atq -d restore_check -c "INSERT INTO marks (name) VALUES ('before')"
LEDGER=$($P -d sms -c "SELECT count(*) || '/' || coalesce(sum(amount), 0) FROM ledger_entries")
sleep 1
TARGET=$($P -c "SELECT now()")
sleep 1
psql -h 127.0.0.1 -p "$(leader_port)" -U postgres -Atq -d restore_check -c "INSERT INTO marks (name) VALUES ('after')"
$P -c "SELECT pg_switch_wal()" >/dev/null
sleep 3 # archive-push (async) ships the switched segment
for repo in 1 2; do
  R=$D/restore-repo$repo
  rm -rf "$R" "$R.log"; mkdir -p "$R"; touch "$R.log"; chown postgres:postgres "$R" "$R.log"; chmod 700 "$R"
  $PGBR --repo=$repo --pg1-path="$R" --type=time "--target=$TARGET" --target-action=promote restore >/dev/null
  setpriv --reuid=postgres --regid=postgres --init-groups -- "$PG_BIN/pg_ctl" -D "$R" -w -t 120 -l "$R.log" \
    -o "-p 5450 -c archive_mode=off -c unix_socket_directories=/tmp -c listen_addresses=127.0.0.1 -c hot_standby=on -c synchronous_standby_names=''" start >/dev/null
  RP="psql -h 127.0.0.1 -p 5450 -U postgres -Atq"
  for _ in $(seq 1 60); do [ "$($RP -c 'SELECT pg_is_in_recovery()' 2>/dev/null)" = f ] && break; sleep 1; done
  marks=$($RP -d restore_check -c "SELECT string_agg(name, ',' ORDER BY at) FROM marks")
  ledger=$($RP -d sms -c "SELECT count(*) || '/' || coalesce(sum(amount), 0) FROM ledger_entries")
  [ "$marks" = before ] && ok "repo$repo point-in-time restore: rows before the target only ($marks)" || bad "repo$repo marks: '$marks'"
  [ "$ledger" = "$LEDGER" ] && ok "repo$repo ledger entries/sum match ($ledger)" || bad "repo$repo ledger $ledger, live $LEDGER"
  setpriv --reuid=postgres --regid=postgres --init-groups -- "$PG_BIN/pg_ctl" -D "$R" -m fast stop >/dev/null
done

echo "== ClickHouse (BACKUP / RESTORE)"
CHQ() { curl -sf -u "sms:$PGPASSWORD" "http://127.0.0.1:8124/" --data-binary "$1"; }
CHD() { curl -sf "http://127.0.0.1:8124/" --data-binary "$1"; }
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
S3="S3('http://127.0.0.1:9001/amaze-backups/clickhouse/$STAMP', 'test', 'test')"
LIVE=$(CHQ "SELECT count() FROM sms.message_records")
CHD "BACKUP DATABASE sms TO $S3" >/dev/null && ok "backup to S3" || bad "backup to S3"
CHD "BACKUP DATABASE sms TO Disk('backups', '$STAMP')" >/dev/null && ok "backup to the backup disk (NFS)" || bad "backup to disk"
for src in "$S3" "Disk('backups', '$STAMP')"; do
  CHD "DROP TABLE IF EXISTS default.restored SYNC" >/dev/null
  CHD "RESTORE TABLE sms.message_records AS default.restored FROM $src" >/dev/null
  got=$(CHD "SELECT count() FROM default.restored")
  [ "$got" = "$LIVE" ] && ok "restore from ${src%%(*}: $got rows" || bad "restore from ${src%%(*}: $got rows, live $LIVE"
done
CHD "DROP TABLE IF EXISTS default.restored SYNC" >/dev/null

echo "== Redis (snapshot)"
R=redis-cli
REP=""
for port in 6380 6381 6382; do
  $R -p $port -a "$PGPASSWORD" --no-auth-warning info replication 2>/dev/null | grep -q '^role:slave' && { REP=$port; break; }
done
before=$($R -p "$REP" -a "$PGPASSWORD" --no-auth-warning LASTSAVE)
$R -p "$REP" -a "$PGPASSWORD" --no-auth-warning BGSAVE >/dev/null
for _ in $(seq 1 120); do [ "$($R -p "$REP" -a "$PGPASSWORD" --no-auth-warning LASTSAVE)" != "$before" ] && break; sleep 0.5; done
KEYS=$($R -p "$REP" -a "$PGPASSWORD" --no-auth-warning DBSIZE)
mkdir -p "$D/nfs/redis" "$D/redis-restore"
cp "$D/redis$((REP - 6380))/dump.rdb" "$D/nfs/redis/dump-$STAMP.rdb"
cp "$D/nfs/redis/dump-$STAMP.rdb" "$D/redis-restore/dump.rdb"
redis-server --port 6390 --dir "$D/redis-restore" --dbfilename dump.rdb --appendonly no --daemonize yes >/dev/null
for _ in $(seq 1 240); do $R -p 6390 info persistence 2>/dev/null | grep -q '^loading:0' && break; sleep 0.25; done
got=$($R -p 6390 DBSIZE)
$R -p 6390 shutdown nosave >/dev/null 2>&1 || true
# Keys with a TTL can expire between the snapshot and the count.
[ "$got" -ge $((KEYS * 99 / 100)) ] && ok "snapshot from replica :$REP restored: $got keys (replica had $KEYS)" || bad "redis restore $got keys, replica $KEYS"

exit $FAIL
