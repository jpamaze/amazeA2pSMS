#!/usr/bin/env bash
# Runs the production HA data tier on one Linux machine, from native
# binaries, to rehearse failovers, backups and restores (docs/phase5-ha.md):
#
#   PostgreSQL 16 x3 under Patroni (etcd x3, synchronous replica),
#     pgBackRest archiving to two repositories: S3 and a local directory
#     standing in for NFS; HAProxy on :5000 (primary) and :5001 (replicas)
#   Redis x3 (one primary, two replicas) with Sentinel x3
#   ClickHouse x3 (one shard, three replicas, embedded Keeper)
#   An S3 server (moto) standing in for S3 / GCS / MinIO
#
#   scripts/ha-local.sh up      # start (fresh data)
#   scripts/ha-local.sh env     # print the app settings for this tier
#   scripts/ha-local.sh status  # who is primary where
#   scripts/ha-local.sh down    # stop (data kept until the next up)
#
# Needs (PATH or the variables): patroni, etcd, pgbackrest, haproxy,
# redis-server, PostgreSQL 16 binaries (PG_BIN), clickhouse (CLICKHOUSE_BIN),
# moto_server (MOTO_BIN), and a `postgres` OS user. Run as root.
# Ports: etcd 2379/2389/2399, PostgreSQL 5441-5443, Patroni 8008-8010,
# HAProxy 5000/5001/7000, Redis 6380-6382, Sentinel 26380-26382,
# ClickHouse HTTP 8124-8126 (native 9301-9303, interserver 9311-9313, Keeper 9181-9183/9234-9236), S3 9000 (TLS) / 9001.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
D=${HA_DIR:-/var/tmp/amaze-ha}
PG_BIN=${PG_BIN:-/usr/lib/postgresql/16/bin}
PATRONI=${PATRONI_BIN:-patroni}
CH=${CLICKHOUSE_BIN:-clickhouse}
MOTO=${MOTO_BIN:-moto_server}
S3_BUCKET=amaze-backups
PASS=${HA_PASSWORD:-sms}

pids() { mkdir -p "$D/pids"; }
start() { # name, command... (as root)
  local name=$1; shift
  pids
  nohup "$@" >"$D/logs/$name.log" 2>&1 &
  echo $! >"$D/pids/$name.pid"
}
start_pg() { # name, command... (as the postgres user)
  local name=$1; shift
  pids
  nohup setpriv --reuid=postgres --regid=postgres --init-groups -- "$@" >"$D/logs/$name.log" 2>&1 &
  echo $! >"$D/pids/$name.pid"
}
wait_for() { # description, command...
  local what=$1; shift
  for _ in $(seq 1 120); do "$@" >/dev/null 2>&1 && return 0; sleep 0.5; done
  echo "timed out waiting for $what" >&2
  return 1
}

stop() {
  pids
  local f
  # Patroni stops its PostgreSQL; give it time before the rest.
  for f in "$D"/pids/patroni*.pid; do [ -e "$f" ] && kill "$(cat "$f")" 2>/dev/null || true; done
  sleep 3
  for f in "$D"/pids/*.pid; do [ -e "$f" ] && kill "$(cat "$f")" 2>/dev/null || true; done
  sleep 2
  for f in "$D"/pids/*.pid; do [ -e "$f" ] && kill -9 "$(cat "$f")" 2>/dev/null || true; done
  # PostgreSQL children of a killed Patroni.
  for n in 1 2 3; do
    [ -f "$D/pg$n/data/postmaster.pid" ] && kill -9 "$(head -1 "$D/pg$n/data/postmaster.pid")" 2>/dev/null || true
  done
  rm -f "$D"/pids/*.pid
}

pg_conf() { # n
  local n=$1 port=$((5440 + $1)) rest=$((8007 + $1))
  cat >"$D/pg$n/pgbackrest.conf" <<EOF
[global]
repo1-type=s3
repo1-path=/pgbackrest
repo1-s3-bucket=$S3_BUCKET
repo1-s3-endpoint=127.0.0.1
repo1-storage-port=9000
repo1-s3-region=us-east-1
repo1-s3-key=test
repo1-s3-key-secret=test
repo1-s3-uri-style=path
repo1-storage-verify-tls=n
repo1-retention-full=2
repo2-type=posix
repo2-path=$D/nfs/pgbackrest
repo2-retention-full=2
start-fast=y
compress-type=zst
process-max=2
log-path=$D/pg$n
lock-path=$D/pg$n/lock
spool-path=$D/pg$n/spool
archive-async=y

[amaze]
pg1-path=$D/pg$n/data
pg1-port=$port
pg1-socket-path=/var/run/postgresql
EOF
  cat >"$D/pg$n/patroni.yml" <<EOF
scope: amaze
name: pg$n
restapi:
  listen: 127.0.0.1:$rest
  connect_address: 127.0.0.1:$rest
  authentication: {username: patroni, password: $PASS}
ctl:
  authentication: {username: patroni, password: $PASS}
etcd3:
  hosts: [127.0.0.1:2379, 127.0.0.1:2389, 127.0.0.1:2399]
  username: root
  password: $PASS
bootstrap:
  dcs:
    ttl: 20
    loop_wait: 5
    retry_timeout: 5
    maximum_lag_on_failover: 1048576
    synchronous_mode: true
    postgresql:
      use_pg_rewind: true
      parameters:
        wal_level: replica
        hot_standby: "on"
        max_wal_senders: 10
        max_replication_slots: 10
        wal_log_hints: "on"
        archive_mode: "on"
        archive_timeout: 60
        max_connections: 200
  initdb: [{encoding: UTF8}, data-checksums]
  pg_hba:
    - local all all trust
    - host replication replicator 127.0.0.1/32 scram-sha-256
    - host all all 127.0.0.1/32 scram-sha-256
postgresql:
  listen: 127.0.0.1:$port
  connect_address: 127.0.0.1:$port
  data_dir: $D/pg$n/data
  bin_dir: $PG_BIN
  pgpass: $D/pg$n/pgpass
  authentication:
    superuser: {username: postgres, password: $PASS}
    replication: {username: replicator, password: $PASS}
    rewind: {username: postgres, password: $PASS}
  parameters:
    unix_socket_directories: /var/run/postgresql
    # PostgreSQL keeps WAL until archiving works; the marker appears once
    # the stanza exists (WAL pushed earlier would make stanza-create fail).
    archive_command: test -f $D/pg$n/stanza.ok && pgbackrest --config=$D/pg$n/pgbackrest.conf --stanza=amaze archive-push "%p"
    restore_command: pgbackrest --config=$D/pg$n/pgbackrest.conf --stanza=amaze archive-get %f "%p"
  create_replica_methods: [pgbackrest, basebackup]
  pgbackrest:
    command: pgbackrest --config=$D/pg$n/pgbackrest.conf --stanza=amaze --delta restore
    keep_data: true
    no_params: true
  basebackup: {checkpoint: fast}
EOF
}

redis_up() {
  local n port
  for n in 0 1 2; do
    port=$((6380 + n))
    mkdir -p "$D/redis$n"
    {
      echo "port $port"
      echo "dir $D/redis$n"
      echo "appendonly yes"
      echo "appendfsync everysec"
      echo "min-replicas-to-write 1"
      echo "min-replicas-max-lag 10"
      echo "requirepass $PASS"
      echo "masterauth $PASS"
      [ "$n" -gt 0 ] && echo "replicaof 127.0.0.1 6380"
    } >"$D/redis$n/redis.conf"
    start "redis$n" redis-server "$D/redis$n/redis.conf"
  done
  for n in 0 1 2; do
    port=$((26380 + n))
    cat >"$D/redis$n/sentinel.conf" <<EOF
port $port
dir $D/redis$n
requirepass $PASS
sentinel sentinel-pass $PASS
sentinel monitor amaze 127.0.0.1 6380 2
sentinel auth-pass amaze $PASS
sentinel down-after-milliseconds amaze 3000
sentinel failover-timeout amaze 20000
sentinel parallel-syncs amaze 1
EOF
    start "sentinel$n" redis-server "$D/redis$n/sentinel.conf" --sentinel
  done
  wait_for "redis" redis-cli -p 6380 -a "$PASS" --no-auth-warning ping
}

ch_up() {
  local n
  for n in 1 2 3; do
    mkdir -p "$D/ch$n/config.d" "$D/ch$n/users.d"
    cat >"$D/ch$n/config.xml" <<EOF
<clickhouse>
  <logger><level>warning</level><log>$D/ch$n/server.log</log><errorlog>$D/ch$n/error.log</errorlog></logger>
  <path>$D/ch$n/data/</path>
  <tmp_path>$D/ch$n/tmp/</tmp_path>
  <user_files_path>$D/ch$n/user_files/</user_files_path>
  <format_schema_path>$D/ch$n/format_schemas/</format_schema_path>
  <user_directories><users_xml><path>users.xml</path></users_xml></user_directories>
  <listen_host>127.0.0.1</listen_host>
  <http_port>$((8123 + n))</http_port>
  <tcp_port>$((9300 + n))</tcp_port>
  <interserver_http_port>$((9310 + n))</interserver_http_port>
  <interserver_http_host>127.0.0.1</interserver_http_host>
  <mark_cache_size>268435456</mark_cache_size>
  <macros><shard>01</shard><replica>ch$n</replica></macros>
  <keeper_server>
    <tcp_port>$((9180 + n))</tcp_port>
    <server_id>$n</server_id>
    <enable_ipv6>false</enable_ipv6>
    <log_storage_path>$D/ch$n/keeper/log</log_storage_path>
    <snapshot_storage_path>$D/ch$n/keeper/snapshots</snapshot_storage_path>
    <coordination_settings><operation_timeout_ms>10000</operation_timeout_ms><session_timeout_ms>30000</session_timeout_ms></coordination_settings>
    <raft_configuration>
      <server><id>1</id><hostname>127.0.0.1</hostname><port>9234</port></server>
      <server><id>2</id><hostname>127.0.0.1</hostname><port>9235</port></server>
      <server><id>3</id><hostname>127.0.0.1</hostname><port>9236</port></server>
    </raft_configuration>
  </keeper_server>
  <zookeeper>
    <node><host>127.0.0.1</host><port>9181</port></node>
    <node><host>127.0.0.1</host><port>9182</port></node>
    <node><host>127.0.0.1</host><port>9183</port></node>
  </zookeeper>
  <remote_servers>
    <amaze>
      <shard>
        <internal_replication>true</internal_replication>
        <replica><host>127.0.0.1</host><port>9301</port><user>sms</user><password>$PASS</password></replica>
        <replica><host>127.0.0.1</host><port>9302</port><user>sms</user><password>$PASS</password></replica>
        <replica><host>127.0.0.1</host><port>9303</port><user>sms</user><password>$PASS</password></replica>
      </shard>
    </amaze>
  </remote_servers>
  <distributed_ddl><path>/clickhouse/task_queue/ddl</path></distributed_ddl>
  <storage_configuration><disks><backups><type>local</type><path>$D/nfs/clickhouse/</path></backups></disks></storage_configuration>
  <backups><allowed_disk>backups</allowed_disk><allowed_path>$D/nfs/clickhouse/</allowed_path></backups>
</clickhouse>
EOF
    cat >"$D/ch$n/users.xml" <<EOF
<clickhouse>
  <profiles><default/></profiles>
  <quotas><default/></quotas>
  <users>
    <default><password></password><networks><ip>127.0.0.1</ip></networks><profile>default</profile><quota>default</quota><access_management>1</access_management></default>
    <sms><password>$PASS</password><networks><ip>127.0.0.1</ip></networks><profile>default</profile><quota>default</quota></sms>
  </users>
</clickhouse>
EOF
    start "ch$n" "$CH" server --config-file="$D/ch$n/config.xml"
  done
  for n in 1 2 3; do wait_for "clickhouse $n" curl -sf "http://127.0.0.1:$((8123 + n))/ping"; done
  # The database is Replicated: tables created in it on any replica (the
  # ch-writer applies the migrations) are created as replicated tables on
  # all three.
  for n in 1 2 3; do
    wait_for "keeper quorum on ch$n" curl -sf "http://127.0.0.1:$((8123 + n))/" --data-binary "SELECT count() FROM system.zookeeper WHERE path = '/'"
    curl -sf "http://127.0.0.1:$((8123 + n))/" --data-binary \
      "CREATE DATABASE IF NOT EXISTS sms ENGINE = Replicated('/clickhouse/databases/sms', '{shard}', '{replica}')" >/dev/null
    curl -sf "http://127.0.0.1:$((8123 + n))/" --data-binary "GRANT ALL ON sms.* TO sms" >/dev/null || true
  done
}

haproxy_up() {
  cat >"$D/haproxy.cfg" <<EOF
global
  maxconn 4000
defaults
  mode tcp
  timeout connect 3s
  timeout client 30m
  timeout server 30m
listen stats
  mode http
  bind 127.0.0.1:7000
  stats enable
  stats uri /
listen s3_tls
  mode http
  bind 127.0.0.1:9000 ssl crt $D/s3.pem
  server s3 127.0.0.1:9001
listen postgres_primary
  bind 127.0.0.1:5000
  option httpchk OPTIONS /primary
  http-check expect status 200
  default-server inter 1s fall 2 rise 2 on-marked-down shutdown-sessions
  server pg1 127.0.0.1:5441 check port 8008
  server pg2 127.0.0.1:5442 check port 8009
  server pg3 127.0.0.1:5443 check port 8010
listen postgres_replicas
  bind 127.0.0.1:5001
  balance roundrobin
  option httpchk OPTIONS /replica
  http-check expect status 200
  default-server inter 1s fall 2 rise 2 on-marked-down shutdown-sessions
  server pg1 127.0.0.1:5441 check port 8008
  server pg2 127.0.0.1:5442 check port 8009
  server pg3 127.0.0.1:5443 check port 8010
EOF
  start haproxy haproxy -db -f "$D/haproxy.cfg"
}

case "${1:-up}" in
up)
  stop || true
  rm -rf "$D"
  mkdir -p "$D/logs" "$D/nfs/pgbackrest" "$D/nfs/clickhouse" "$D/nfs/redis" /var/run/postgresql
  chown postgres:postgres /var/run/postgresql

  # S3 on :9001, behind HAProxy terminating TLS on :9000 (pgBackRest talks
  # to S3 over HTTPS only), and the bucket.
  start s3 "$MOTO" -H 127.0.0.1 -p 9001
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj /CN=127.0.0.1 \
    -keyout "$D/s3.key" -out "$D/s3.crt" >/dev/null 2>&1
  cat "$D/s3.crt" "$D/s3.key" >"$D/s3.pem"
  haproxy_up
  wait_for "s3" curl -sfk https://127.0.0.1:9000/
  curl -sfk -X PUT "https://127.0.0.1:9000/$S3_BUCKET" >/dev/null

  # etcd x3 for Patroni.
  for n in 0 1 2; do
    c=$((2379 + n * 10)); p=$((2380 + n * 10))
    start "etcd$n" etcd --name "e$n" --data-dir "$D/etcd$n" \
      --listen-client-urls "http://127.0.0.1:$c" --advertise-client-urls "http://127.0.0.1:$c" \
      --listen-peer-urls "http://127.0.0.1:$p" --initial-advertise-peer-urls "http://127.0.0.1:$p" \
      --initial-cluster "e0=http://127.0.0.1:2380,e1=http://127.0.0.1:2390,e2=http://127.0.0.1:2400" \
      --initial-cluster-state new --initial-cluster-token amaze
  done
  wait_for "etcd" etcdctl --endpoints=127.0.0.1:2379 endpoint health
  # As in production: etcd holds the cluster's configuration, so it needs auth.
  ETCDCTL_API=3 etcdctl --endpoints=127.0.0.1:2379 user add root --new-user-password="$PASS" --interactive=false >/dev/null
  ETCDCTL_API=3 etcdctl --endpoints=127.0.0.1:2379 user grant-role root root >/dev/null
  ETCDCTL_API=3 etcdctl --endpoints=127.0.0.1:2379 auth enable >/dev/null

  # PostgreSQL under Patroni: pg1 bootstraps, the others clone it.
  for n in 1 2 3; do mkdir -p "$D/pg$n/lock" "$D/pg$n/spool"; pg_conf "$n"; done
  chown -R postgres:postgres "$D"/pg* "$D/nfs/pgbackrest"
  start_pg patroni1 "$PATRONI" "$D/pg1/patroni.yml"
  wait_for "pg1 primary" curl -sf http://127.0.0.1:8008/primary
  runuser -u postgres -- pgbackrest --config="$D/pg1/pgbackrest.conf" --stanza=amaze stanza-create >"$D/logs/stanza.log" 2>&1 ||
    { cat "$D/logs/stanza.log" >&2; exit 1; }
  for n in 1 2 3; do runuser -u postgres -- touch "$D/pg$n/stanza.ok"; done
  start_pg patroni2 "$PATRONI" "$D/pg2/patroni.yml"
  start_pg patroni3 "$PATRONI" "$D/pg3/patroni.yml"
  wait_for "pg2 replica" curl -sf http://127.0.0.1:8009/replica
  wait_for "pg3 replica" curl -sf http://127.0.0.1:8010/replica
  psql -h 127.0.0.1 -p 5441 -U postgres -q -c "CREATE ROLE sms LOGIN PASSWORD '$PASS'" -c "CREATE DATABASE sms OWNER sms" \
    2>&1 | grep -v "^$" || true
  redis_up
  ch_up
  echo "HA tier up in $D"
  "$0" status
  ;;
env)
  cat <<EOF
export POSTGRES_URL='postgres://sms:$PASS@127.0.0.1:5441,127.0.0.1:5442,127.0.0.1:5443/sms?sslmode=disable&target_session_attrs=read-write&connect_timeout=3'
export REDIS_ADDRS=127.0.0.1:26380,127.0.0.1:26381,127.0.0.1:26382 REDIS_SENTINEL_MASTER=amaze REDIS_PASSWORD=$PASS REDIS_SENTINEL_PASSWORD=$PASS
export CLICKHOUSE_URL=http://127.0.0.1:8124,http://127.0.0.1:8125,http://127.0.0.1:8126 CLICKHOUSE_PASSWORD=$PASS
export PGPASSWORD=$PASS
EOF
  ;;
status)
  echo "PostgreSQL (Patroni):"
  "$PATRONI" --version >/dev/null 2>&1 && runuser -u postgres -- "$(dirname "$(command -v "$PATRONI")")/patronictl" -c "$D/pg1/patroni.yml" list 2>/dev/null || true
  echo "Redis primary (Sentinel): $(redis-cli -p 26380 -a "$PASS" --no-auth-warning SENTINEL get-master-addr-by-name amaze 2>/dev/null | paste -sd:)"
  for n in 1 2 3; do
    printf 'ClickHouse ch%s: %s\n' "$n" "$(curl -s "http://127.0.0.1:$((8123 + n))/" --data-binary "SELECT 'up, readonly replicas: ' || toString(countIf(is_readonly)) FROM system.replicas" 2>/dev/null || echo down)"
  done
  ;;
down)
  stop
  ;;
*)
  echo "usage: $0 up|env|status|down" >&2
  exit 2
  ;;
esac
