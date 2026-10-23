# Phase 5 slice 1: high availability, backups and failover drills

The blueprint's last phase is hardening: "failover drills, DR backups, security audit, 10,000 TPS 24-hour soak test", and its reliability rule 10, "no single points of failure". This slice covers the first two:
- **A replicated data tier on VMs**, run by us (decided: self-run VMs, no managed databases).
- **Backups** to S3-compatible storage *and* to an NFS share, with tested restores.
- **Failover drills** that crash each store under load and check that no money or message is lost.

Slice 2 covers the security audit and the 10,000 TPS 24-hour soak.

## Production layout

| VMs | Runs | Fails over by | Compose file |
| --- | --- | --- | --- |
| pg-1..3 | PostgreSQL 16 under Patroni, etcd, pgBackRest | Patroni promotes the synchronous replica (etcd decides the leader) | `deploy/ha/postgres.yml` |
| redis-1..3 | Redis 7.2 + Sentinel, redis_exporter (metrics) | Sentinel (quorum 2 of 3) promotes a replica; clients follow it | `deploy/ha/redis.yml` |
| ch-1..3 | ClickHouse 24.8, one shard, three replicas, embedded Keeper | none needed: every replica holds all data; the platform moves to another replica | `deploy/ha/clickhouse.yml` |
| kafka-1..3 | Redpanda, replication 3 | Raft leader election per partition | `deploy/vm/kafka.yml` |
| app-1..3 | every platform service | the load balancer drops a VM whose `/readyz` fails; Kafka moves its partitions | `deploy/vm/app.yml` |
| lb-1..2 | HAProxy (HTTPS on 443 by host name: HTTP API, HTTP-vendor receipts, portal; SMPP; PostgreSQL for operators) + keepalived (virtual IP) | VRRP moves the IP; on a public cloud use its network load balancer instead | `deploy/ha/lb.yml` |
| mon-1 | Prometheus, Alertmanager (alerts by e-mail), Grafana | — | `deploy/vm/monitoring.yml` |

Put the three VMs of each tier in different availability zones (or racks). `deploy/ha/env.example` lists every address and secret; the same `.env` goes on every VM.

**How the services find the primaries:**
- **PostgreSQL:** `POSTGRES_URL` names all three nodes with `target_session_attrs=read-write`; the driver connects to the writable one and reconnects after a failover. HAProxy (`:5000` primary, `:5001` replicas, using Patroni's health endpoints) is there for operators and tools.
- **Redis:** `REDIS_ADDRS` lists the Sentinels and `REDIS_SENTINEL_MASTER=amaze` names the primary they watch (new settings, plus `REDIS_PASSWORD`).
- **ClickHouse:** `CLICKHOUSE_URL` lists all three replicas, comma-separated. A request that cannot connect, or finds a replica read-only (lost its Keeper quorum), goes to the next; the client stays with the one that answered.

## What a failure costs

| Fails | Detected and replaced in | Customer impact | Data |
| --- | --- | --- | --- |
| PostgreSQL primary | `ttl` 30 s (measured 18–20 s with the drill's 20 s) | none: the message path does not use PostgreSQL; rating, top-ups and the portal wait and retry | nothing lost: one replica is synchronous (`synchronous_mode`), so every committed transaction is on the new primary |
| Redis primary | Sentinel `down-after` 5 s + election (measured 4.3–4.7 s with 3 s) | submits refused for those seconds with a temporary error (SMPP `ESME_RMSGQFUL`, HTTP 503); clients retry | the last writes not yet on a replica can be lost: see "Redis after a failover" |
| ClickHouse replica | immediately (next request) | none | none; the replica catches up when it returns |
| two ClickHouse replicas (Keeper quorum) | — | none; reports and history are read-only until the quorum is back | history waits in Kafka (72 h) and is written when the quorum returns |
| Kafka broker | Redpanda leader election, seconds | none (acks from a majority) | none |
| app VM | load-balancer health check, 6 s | SMPP binds on that VM reconnect to another; HTTP requests in flight are retried by the client | none: nothing is acknowledged before it is in Kafka |

### Redis after a failover

Redis replicates asynchronously. A primary that dies can take the last moments of writes with it. The design keeps that small and repairs it:

- **Bounding the loss:** `min-replicas-to-write 1` (at most 10 s behind) makes a primary cut off from its replicas refuse writes rather than accept writes a failover would lose.
- **Message records rebuilt:** a message whose Redis record was lost still travels whole through Kafka. When the vendor accepts it, the connector rebuilds the record from the message before recording the vendor message IDs, so its receipt still matches. It retries that for 20 s through a failover instead of giving up.
- **Wallets repaired from PostgreSQL:** the PostgreSQL ledger is exactly-once and is the system of record. `wallet-resync` rebuilds every wallet's balance, credit limit and held amount in Redis from it, and drops holds of messages that were sent long ago (their charge is already in the ledger). It is safe while traffic flows:
  - it holds a PostgreSQL advisory lock exclusively;
  - every change made to a balance in both stores (rater batches, top-ups, transfers, refunds) holds the same lock shared, so a resync never sees one side changed without the other;
  - holds placed meanwhile are recounted atomically.

  Run it after every Redis failover or restore:
  ```sh
  docker compose -f deploy/vm/app.yml --env-file .env --profile ops run --rm wallet-resync -dry-run   # report
  docker compose -f deploy/vm/app.yml --env-file .env --profile ops run --rm wallet-resync            # repair
  ```
- **History rows:** ClickHouse history rows without valid IDs (from a record rebuilt from partial state) are skipped and counted (`sms_records_dropped_total`) instead of blocking the writer.

### Services through a failover

A consumer whose batch fails (for example, the database it writes to is failing over) retries the same batch in place for up to 2 minutes, with backoff, before the service exits to be restarted (`sms_consumer_batch_retries_total`). Before this, the rater exited at the first error of a PostgreSQL failover; nothing was lost, but nothing was rated until a restart.

## Backups

Every store backs up to both destinations. Each is a complete copy, so either alone can restore the platform. The cron schedule is `deploy/ha/backup.cron`.

| Store | Tool | Schedule | S3 | NFS | Restore point |
| --- | --- | --- | --- | --- | --- |
| PostgreSQL | pgBackRest, two repositories, AES-256 encrypted | WAL archived continuously; full on Sunday, differential on other days (current leader only) | repo1, 4 fulls kept | repo2, 2 fulls kept | any moment since the oldest full (point in time) |
| ClickHouse | `BACKUP DATABASE` | full on Sunday, incremental daily (CH_1) | `clickhouse/` prefix (expire with a bucket lifecycle rule) | `/backup/clickhouse`, 35 days | the backup time |
| Redis | RDB snapshot from a replica | hourly | `redis/` | `/backup/redis`, 35 days | the snapshot time, then `wallet-resync` |
| Kafka | not backed up: replicated 3 ways, 72 h retention; everything in it ends up in PostgreSQL or ClickHouse | | | | |

- **S3:** any S3-compatible store: AWS S3, Google Cloud Storage (interoperability keys), or MinIO in your own data centre (`S3_URI_STYLE=path`).
- **NFS:** a share from a machine outside the platform's failure domain, mounted at `BACKUP_NFS_PATH` on the PostgreSQL, Redis and ClickHouse VMs. Any mounted disk works; the scripts only need a path.
- **Encryption key:** keep `BACKUP_CIPHER_PASS` outside the platform as well (a password manager or a sealed envelope). PostgreSQL backups cannot be restored without it.

### Setting up backups

1. Create the bucket, and an access key limited to it. Add a lifecycle rule expiring `clickhouse/` and `redis/` after 35 days.
2. Mount the NFS share at `BACKUP_NFS_PATH` on the PostgreSQL, Redis and ClickHouse VMs. Create `pgbackrest/` (owned by uid 999, postgres in the image), `clickhouse/` (uid 101) and `redis/`.
3. After the PostgreSQL cluster first starts, on the leader (`docker compose -f postgres.yml exec patroni patronictl -c /etc/amaze/patroni.yml list`):
   ```sh
   docker compose -f deploy/ha/postgres.yml --env-file deploy/ha/.env exec patroni /etc/amaze/backup.sh init
   ```
   This creates the pgBackRest stanza in both repositories and takes the first full backups. WAL archiving starts once the stanza exists (until then PostgreSQL keeps the WAL).
4. Install `deploy/ha/backup.cron` on the VMs it names.
5. Alert on backups: `pg_stat_archiver.failed_count` rising, `backup.sh info` older than 8 days, and `system.backups` errors. The rules in `deploy/vm/monitoring/alerts.yml` do not cover backups yet, so add a check of your own.

### Restoring

**PostgreSQL to a point in time** (for example, just before a bad change at 14:02):
1. Stop Patroni on all three nodes: `docker compose -f deploy/ha/postgres.yml stop patroni`.
2. Remove the cluster from etcd: `patronictl -c /etc/amaze/patroni.yml remove amaze`.
3. On pg-1, restore from S3 (or `--repo=2` from NFS):
   ```sh
   docker compose -f deploy/ha/postgres.yml run --rm --entrypoint bash patroni -c \
     'envsubst </etc/amaze/pgbackrest.conf.tmpl >/tmp/pgbr.conf && pgbackrest --config=/tmp/pgbr.conf --stanza=amaze --repo=1 --delta \
      --type=time "--target=2026-10-21 14:01:59+05:30" --target-action=promote restore'
   ```
4. Start Patroni on pg-1, then on pg-2 and pg-3: they clone it.
5. After any restore of PostgreSQL, run `wallet-resync`.

**ClickHouse:**
```sql
RESTORE DATABASE sms FROM S3('https://<bucket>.<endpoint>/clickhouse/<name>', '<key>', '<secret>')
-- or FROM Disk('backups', '<name>')
```
Run this on one replica after dropping the damaged database on all of them. Or restore a single table under a new name (`RESTORE TABLE sms.message_records AS sms.message_records_restored FROM ...`) and copy rows back. Messages accepted after the backup are rewritten from Kafka if the ch-writer group is reset to the backup time; beyond Kafka's 72 h they are gone from history (money is in PostgreSQL).

**Redis** (only if all three nodes lost their data):
1. Stop the three Redis containers.
2. Copy the newest `redis-*.rdb` into the primary's volume as `dump.rdb`, with `appendonly no` for the first start.
3. Start it, then the replicas, then run `wallet-resync`. Receipts for messages sent after the snapshot will not match.

## Rehearsing it on one machine

`scripts/ha-local.sh` runs the whole data tier from native binaries on one Linux machine:
- PostgreSQL ×3 with Patroni, etcd ×3, pgBackRest;
- Redis ×3 with Sentinel ×3;
- ClickHouse ×3 with Keeper;
- HAProxy;
- an S3 server (moto) with a directory standing in for NFS.

`scripts/dev-local.sh up` then runs the platform on it, and `scripts/ha-drill.sh` and `scripts/ha-restore-check.sh` run the drills and the restore checks.

```sh
scripts/ha-local.sh up
eval "$(scripts/ha-local.sh env)"
scripts/dev-local.sh up
scripts/ha-drill.sh all          # postgres | redis | clickhouse | clickhouse-quorum
scripts/ha-restore-check.sh
```

The tools it needs are listed at the top of `scripts/ha-local.sh` (in the Ubuntu archive: `pgbackrest haproxy etcd-server`; Patroni from pip; ClickHouse's static binary from its GitHub releases).

## Verification

**Drills** (`scripts/ha-drill.sh all`): SMPP load at 500 TPS for 90 s against the local HA tier, the failure at +20 s, the node brought back, then `billing-check`. Final run:

| Drill | Failover | Submits refused | Receipts | Afterwards | billing-check |
| --- | --- | --- | --- | --- | --- |
| PostgreSQL leader crash (Patroni + PostgreSQL killed with -9) | new leader 20.0 s; writes through the multi-host URL 21.8 s, through HAProxy 22.2 s | 0 of 34,161 | all | old leader back as a replica via pg_rewind | PASS |
| Redis primary crash | Sentinel promoted a replica in 4.4 s | 2,231 (temporary error, the 4.4 s at about 500 TPS) | all 29,924 | old primary back as a replica; `wallet-resync` found nothing to repair | PASS |
| ClickHouse replica crash | writer moved to the next replica at once | 0 | all | the replica caught up: 457,014 rows on all three | PASS |
| Two ClickHouse replicas down 30 s (Keeper quorum lost) | ch1 read-only; history waited in Kafka; writable again 40.6 s after the stop | 0 | all | 497,480 rows on all three | PASS |

Everything runs on one 4-vCPU machine (three ClickHouse, three PostgreSQL, three Redis and the whole platform), so throughput and latency in the drills show CPU contention, not the platform; they are not a performance result.

**What the drills found and fixed:**
- **Rater exit:** the rater exited at the first error of a PostgreSQL failover, so nothing was rated until a restart. Consumers now retry a failed batch in place for up to 2 minutes.
- **Redis records and ClickHouse:**
  - A message record lost in a Redis failover was half rebuilt by the connector's vendor-ID write (no account), and the history row made from it stopped the ClickHouse writer for good.
  - The connector now rebuilds the whole record from the message and retries through the failover.
  - The writer skips rows without valid IDs.
  - Before the fix 70 receipts were lost in the drill; after it, none.
- **Receipt replay:** a receipt counted by a batch that then failed was treated as a duplicate on retry, so the message never got its final status or the customer's receipt (16 in a drill). A replayed receipt now completes the message.
- **billing-check timing:** billing-check judged ClickHouse once, right after the load; it now waits (up to `-settle`) for history to catch up, as it already did for balances.

**Backups and restores** (`scripts/ha-restore-check.sh`), all PASS:
- **PostgreSQL:** full backup to S3 and to the NFS directory. A point-in-time restore from each repository into a scratch instance has the row written before the target time and not the one written after, with the ledger identical (251 entries, same total).
- **ClickHouse:** `BACKUP DATABASE` to S3 and to the backup disk. `RESTORE` of `message_records` from each gave the live row count (356,589).
- **Redis:** a snapshot from a replica, loaded into a scratch Redis, had all 165,306 keys.

**Go tests** (`go test -race ./...`, three runs, nothing skipped):
- `wallet-resync`: drift and stale holds repaired, dry run changes nothing, and it waits for a balance change holding the lock.
- ClickHouse client: failover across replicas (refused, read-only, sticky, query errors not retried), Replicated engine rewriting, and the writer skipping rows without IDs.
- Message record rebuilt from the message; the connector's vendor-ID mapping riding out a Redis outage.
- A replayed receipt completing its message.
- Consumer retry: rides out failures, gives up after `RetryFor`, stops with the service.

**Configuration:** `docker compose config` resolves every `deploy/ha` file and `deploy/vm/app.yml` with both the HA and the sign-off `.env`. The rendered Patroni configuration passes `patroni --validate-config` (apart from the addresses of the test machine), and the HAProxy configuration passes `haproxy -c`. The Docker images themselves were not built here (no Docker daemon).
