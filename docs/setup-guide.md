# Production setup guide

This guide is for the sysadmin who installs the platform on the operator's own VMs. The chosen production target:
- **VMs** run by us (no managed databases), one role per VM.
- **Docker Compose** on each VM, using the files in `deploy/vm/` and `deploy/ha/`.
- **The HA layout** from [phase5-ha.md](phase5-ha.md): every data store replicated, three app VMs behind two load balancers.
- **Backups** to S3-compatible storage *and* to an NFS share.

It collects what is spread over [architecture.md](architecture.md), [signoff-5k.md](signoff-5k.md), [phase5-ha.md](phase5-ha.md), [phase5-security.md](phase5-security.md), [soak-test.md](soak-test.md), [admin-auth.md](admin-auth.md) and [phase4-portals.md](phase4-portals.md) into one order of work. Those documents stay the reference for why things are built the way they are.

Two warnings before you start:
- **Not yet run end to end on Docker.** The `deploy/ha` files were checked with `docker compose config`, `patroni --validate-config` and `haproxy -c`, and the same setup was rehearsed from native binaries on one machine (`scripts/ha-local.sh`). The HA Docker images themselves have not been built and run together yet (phase5-ha.md, Verification). Expect to fix small things on the first install, and do the first install on staging VMs.
- **Some pieces are not in the repository.** The Keycloak server itself, certificate renewal, backup alerts, log rotation and a production restore check have to be added by you. Each is called out where it comes up, and listed in [Gaps](#gaps-in-the-repository).

Contents:
1. [Overview](#1-overview)
2. [Prerequisites](#2-prerequisites)
3. [Install, step by step](#3-install-step-by-step)
4. [Verify the installation](#4-verify-the-installation)
5. [Day-2 operations](#5-day-2-operations)
6. [Single-machine trial and troubleshooting](#6-single-machine-trial-and-troubleshooting)
7. [Gaps in the repository](#gaps-in-the-repository)

---

## 1. Overview

### What the platform does

Customers send SMS over SMPP (3.3/3.4/5.0) or HTTP. The platform checks India DLT, the firewall, number portability and the wallet, and writes the message to Kafka before it acknowledges. It then routes the message to a vendor over SMPP or HTTP, and returns the delivery receipt to the customer (SMPP deliver_sm or a signed webhook). Charges go to a PostgreSQL ledger, and message history goes to ClickHouse. Portals for the platform, resellers and customers are served by admin-api.

```
Customer SMPP ─▶ smpp-server ─┐
Customer HTTP ─▶ http-api ────┴─▶ sms.submit ─▶ router ─▶ sms.route.<vendor> ─▶ connector ─▶ vendor
                                                  │                                │
                                              sms.status ◀─────────────────────────┘
                                   rater (ledger)     dlr-processor (state, receipts) ◀── sms.dlr
```

### Services

Every service is one Go binary and one image (`Dockerfile`, `--build-arg SERVICE=<name>`). All of them run on every app VM.

| Service | What it does | Ops port in `app.yml` | Other listeners |
| --- | --- | --- | --- |
| smpp-server | Customer SMPP edge: binds, checks, balance hold, write to Kafka, receipts back as deliver_sm | 9101 | 2775 (`SMPP_ADDR`) |
| http-api | Customer HTTP edge (`/v1/sms`, `api/openapi.yaml`) | 9102 | 8080 (`HTTP_ADDR`) |
| admin-api | Admin and portal API, serves the portal (Next.js static export built into its image) | 9103 | 8081 (`ADMIN_ADDR`) |
| router | Picks a vendor (least cost, priority, weighted), HLR lookups, failover | 9104 | — |
| rater | Settles holds, writes the ledger in batches | 9105 | — |
| dlr-processor | Matches vendor receipts, message state, customer receipts, expiry | 9106 | — |
| connector | Sends to vendors over SMPP or HTTP. One `connector` service runs every vendor that is not disabled and has an enabled bind or an HTTP profile (`VENDOR_ID=*`) | 9107 | 8090 (`DLR_ADDR`, receipts from HTTP vendors at `/dlr/<vendor>`) |
| webhook-dispatcher | Signed delivery-status webhooks to HTTP customers, with retries | 9109 | — |
| ch-writer | Message history to ClickHouse; creates the ClickHouse tables | 9110 | — |
| alerter | Low-balance alerts by e-mail, SMS and webhook | 9111 | — |

One-off and operator tools: `migrate` (PostgreSQL schema), `admin-user` (platform account and first platform owner), `wallet-resync` (repairs Redis wallets from the ledger), `billing-check` (money and history cross-check), `soak-report` (judges a soak from Prometheus), `devseed` (demo data, refuses `APP_ENV=production`), `smpp-load`, `smpp-sim`, `http-sim` (load generator and vendor simulators).

Every service serves `/healthz` (process up), `/readyz` (every dependency reachable, else 503 with a JSON body naming them) and `/metrics` on its ops port.

### VM roles (HA layout)

| VMs | `.env` names | Runs | Compose file | Fails over by |
| --- | --- | --- | --- | --- |
| lb-1, lb-2 | `LB_1`, `LB_2`, `LB_VIP` | HAProxy + keepalived (virtual IP) | `deploy/ha/lb.yml` | VRRP moves the IP |
| app-1..3 | `APP_1`..`APP_3` | every platform service | `deploy/vm/app.yml` | the balancer drops a VM whose `/readyz` fails; Kafka moves its partitions |
| pg-1..3 | `PG_1`..`PG_3` | PostgreSQL 16 under Patroni, etcd, pgBackRest | `deploy/ha/postgres.yml` | Patroni promotes the synchronous replica |
| redis-1..3 | `REDIS_1`..`REDIS_3` | Redis 7.2 + Sentinel | `deploy/ha/redis.yml` | Sentinel (quorum 2 of 3) |
| ch-1..3 | `CH_1`..`CH_3` | ClickHouse 24.8, one shard, three replicas, embedded Keeper | `deploy/ha/clickhouse.yml` | clients move to the next replica |
| kafka-1..3 | `KAFKA_1`..`KAFKA_3` | Redpanda (Kafka API), replication 3 | `deploy/vm/kafka.yml` | Raft leader election |
| mon-1 | `MON_1` | Prometheus, Alertmanager and Grafana | `deploy/vm/monitoring.yml` | — |
| sim-1 | `SIM_1` | vendor simulators and load generator (sign-off, drills, soak only) | `deploy/vm/sim.yml` | — |

Put the three VMs of each tier in different availability zones or racks. On a public cloud, use its network load balancer (TCP pass-through) instead of lb-1/lb-2 and skip `lb.yml`; that balancer then has to terminate HTTPS and route by host name as `haproxy.cfg` does (step 3.12).

### Sizing

The repository gives sizes for the single-node sign-off layout ([signoff-5k.md](signoff-5k.md)); the HA sizes are in the blueprint, not in this repository. Use the sign-off sizes as the floor:

| Role | Sign-off size | Notes for HA |
| --- | --- | --- |
| Kafka (Redpanda) | 8 vCPU, 32 GB, 500 GB NVMe | `REDPANDA_SMP=6`, `REDPANDA_MEMORY=24G`; 72 h retention at three copies |
| PostgreSQL | 8 vCPU, 32 GB, 500 GB NVMe | HA defaults: `PG_SHARED_BUFFERS=16GB`, `effective_cache_size` fixed at 20GB in `patroni.yml.tmpl` |
| Redis | (shared with PostgreSQL in the sign-off) | HA default `REDIS_MAXMEMORY=24gb`; add 2–3 GB per 100 million ported numbers (MNP) |
| ClickHouse | 8 vCPU, 32 GB, 1 TB NVMe | about 45 GB a day at 5,000 TPS, 90 days online (ADR 0001); keep 100 GB free per replica for a soak |
| App | 8 vCPU, 16 GB | three in HA |
| Load balancer | not given | HAProxy runs with `maxconn 200000` |
| Monitoring | 4 vCPU, 8 GB, 200 GB | Prometheus keeps 30 days |
| Simulator / load generator | 8 vCPU, 16 GB | 16 vCPU for the 10,000 TPS soak |

### Network and firewall

All VMs sit in one private network. Only the load balancers' virtual IP is reachable from the internet. Services run with `network_mode: host`, so each port below is a port on the VM itself.

| Port | On | Service | Who may reach it |
| --- | --- | --- | --- |
| 2775 | `LB_VIP` | SMPP (plain), to app VMs with PROXY protocol v2 | customers (internet) |
| 3550 | `LB_VIP` | SMPP over TLS, terminated by HAProxy (`certs/smpp.pem`) | customers (internet) |
| 443 | `LB_VIP` | HTTPS, terminated by HAProxy (`certs/https/`). By host name: `API_HOST` → HTTP API, `DLR_HOST` → HTTP-vendor receipts, every other name → portal and admin API | customers, staff, resellers, HTTP vendors (internet) |
| 80 | `LB_VIP` | redirects to HTTPS only | internet |
| 8080, 8081 | `LB_1`/`LB_2` private IP | plain HTTP API and portal, for operators and tools | operators |
| 7000 | `LB_1`/`LB_2` private IP | HAProxy stats (user `admin`, `LB_STATS_PASSWORD`) | operators |
| 7001 | `LB_1`/`LB_2` private IP | HAProxy Prometheus metrics (`/metrics`) | `MON_1` |
| 5000, 5001 | `LB_1`/`LB_2` private IP | PostgreSQL primary / replicas, for operators and tools | operators |
| VRRP (IP protocol 112) | between `LB_1` and `LB_2` | keepalived | the two LB VMs |
| 2775, 8080, 8081 | app VMs | smpp-server, http-api, admin-api | LB VMs; `SIM_1` for load tests |
| 8090 | app VMs | connector: HTTP-vendor receipts (`/dlr/<vendor>?token=…`) | LB VMs (vendors reach it as `https://DLR_HOST/dlr/<vendor>`) |
| 9101–9107, 9109–9111 | app VMs | ops: `/healthz`, `/readyz`, `/metrics` | LB VMs (health checks on 9101, 9102, 9103, 9107), `MON_1` |
| 5432 | PG VMs | PostgreSQL | app VMs, other PG VMs, LB VMs |
| 8008 | PG VMs | Patroni REST (health and `/metrics` open; changes need `PATRONI_REST_PASSWORD`) | PG and LB VMs, and `MON_1` (metrics) |
| 2379, 2380 | PG VMs | etcd client and peer | PG and LB VMs only (as `postgres.yml` says) |
| 6379 | Redis VMs | Redis | app VMs, other Redis VMs |
| 26379 | Redis VMs | Sentinel | app VMs, other Redis VMs |
| 9121 | Redis VMs | redis_exporter (metrics) | `MON_1` |
| 8123 | CH VMs | ClickHouse HTTP | app VMs, other CH VMs |
| 9181, 9234 | CH VMs | Keeper client, Keeper Raft | other CH VMs |
| 9363 | CH VMs | ClickHouse Prometheus metrics | `MON_1` |
| ClickHouse interserver port | CH VMs | replication (ClickHouse default 9009; not set in the repository) | other CH VMs |
| 9092 | Kafka VMs | Kafka API | app VMs, `SIM_1`, other Kafka VMs |
| 33145 | Kafka VMs | Redpanda RPC | other Kafka VMs |
| 9644 | Kafka VMs | Redpanda admin and metrics | `MON_1` |
| 9100 | every VM | node-exporter | `MON_1` |
| 9090 | `MON_1` | Prometheus | operators; app VMs (admin-api live monitor reads `http://MON_1:9090`) |
| 9093 | `MON_1` | Alertmanager | operators (Prometheus reaches it on 127.0.0.1) |
| 3000 | `MON_1` | Grafana | operators |
| 2776, 2777 | `SIM_1` | SMPP and HTTP vendor simulators | app VMs |

`MON_1` also needs outbound access to the SMTP relay (`SMTP_HOST:SMTP_PORT`) and, if set, to `ALERT_WEBHOOK_URL`. Nothing inside the private network is encrypted (PostgreSQL uses `sslmode=disable`; Redis, Kafka and ClickHouse are plain). That is accepted while the network carries nothing else (phase5-security.md, "Still open").

---

## 2. Prerequisites

| Item | What you need |
| --- | --- |
| OS | Linux on every VM. Kernel limits raised (step 3.1). |
| Docker | Docker Engine 26+ and the Compose plugin on every VM, run by a user that cron can use too. |
| Repository | A checkout on every VM. This guide uses `/opt/amaze`, the path `deploy/ha/backup.cron` expects. |
| Images | Either build on each VM (`docker compose ... build`) or build once and push to your registry (`REGISTRY`, `TAG`). CI (`.github/workflows/ci.yml`, job `images`) builds every image but does not push them. |
| DNS | A records pointing at `LB_VIP`'s public address (or the cloud balancer): one for SMPP, the portal's name, `API_HOST` (customers' HTTP API), `DLR_HOST` (HTTP vendors' receipts), plus one per reseller white-label domain (the reseller points a CNAME at the portal host). You choose the names; set `API_HOST` and `DLR_HOST` in `.env`. |
| TLS certificates | **SMPP 3550:** chain followed by the private key, saved as `deploy/ha/lb/certs/smpp.pem` on both LB VMs. **HTTPS 443:** one PEM per name (chain followed by key) in `deploy/ha/lb/certs/https/` on both LB VMs: the portal's name, `API_HOST`, `DLR_HOST` and every reseller domain. HAProxy refuses to start while `https/` has none. Never commit the keys. |
| Keycloak | A Keycloak server you run (the repository only has the dev one in `deploy/docker-compose.yml`, version 25.0.6, and the realm file `deploy/keycloak/amaze-realm.json`). Browsers and admin-api must reach it over HTTPS. |
| S3 bucket | Any S3-compatible store (AWS S3, Google Cloud Storage with interoperability keys, MinIO with `S3_URI_STYLE=path`). One bucket, an access key limited to it, and a lifecycle rule expiring `clickhouse/` and `redis/` after 35 days. |
| NFS share | From a machine outside the platform's failure domain, mounted at `BACKUP_NFS_PATH` (default `/mnt/backup`) on the PG, Redis and CH VMs. Any mounted disk works; the scripts only need a path. |
| SMTP relay | For monitoring alerts (Alertmanager e-mails `ALERT_EMAIL_TO`) and low-balance alert e-mails to customers. |
| HLR provider | Optional; without `HLR_URL`, MNP and number series still apply. |
| Secrets store | Somewhere outside the platform for `BACKUP_CIPHER_PASS` (a password manager or a sealed envelope): PostgreSQL backups cannot be restored without it. |

---

## 3. Install, step by step

### 3.1 Prepare every VM

1. Install Docker Engine 26+ and the Compose plugin.
2. Clone the repository to `/opt/amaze`:
   ```sh
   sudo git clone https://github.com/jpamaze/amazea2psms /opt/amaze
   ```
3. Raise kernel limits (make them permanent in `/etc/sysctl.d/` as well):
   ```sh
   sudo sysctl -w net.core.somaxconn=65535 fs.file-max=2097152 net.ipv4.ip_local_port_range="10000 65000"
   ```
4. Open the firewall as in the [network table](#network-and-firewall).
5. On the PG, Redis and CH VMs, mount the NFS share at `BACKUP_NFS_PATH` and create its folders (once, from any of them):
   ```sh
   sudo mkdir -p /mnt/backup/pgbackrest /mnt/backup/clickhouse /mnt/backup/redis
   sudo chown 999 /mnt/backup/pgbackrest     # postgres in the image
   sudo chown 101 /mnt/backup/clickhouse     # clickhouse in the image
   ```
6. Start host metrics on every VM:
   ```sh
   cd /opt/amaze/deploy/vm && docker compose -f node-exporter.yml up -d
   ```

### 3.2 Write the `.env` file

1. Copy `deploy/ha/env.example` to `deploy/ha/.env`, fill it in, and copy the same file to `deploy/vm/.env`. **The same file goes on every VM, in both places.** The app, Kafka and monitoring files in `deploy/vm` read the same variables.
2. Fill in the public names (`API_HOST`, `DLR_HOST`), sign-in (`OIDC_ISSUER`, step 3.9), where monitoring alerts go (`ALERT_EMAIL_TO`, and the `SMTP_*` relay) and, if you use them, the `HLR_*` and `ALERT_SMS_*` settings. `deploy/ha/env.example` has all of them.
3. Generate the secrets:
   ```sh
   openssl rand -hex 32        # SECRETS_KEY (exactly 64 hex characters)
   openssl rand -base64 48     # BACKUP_CIPHER_PASS
   openssl rand -hex 24        # each password
   ```
   Hex passwords avoid characters that would need escaping inside `POSTGRES_URL`.
4. Keep a copy of `SECRETS_KEY` and `BACKUP_CIPHER_PASS` outside the platform. Vendor passwords and webhook secrets are sealed with `SECRETS_KEY`; losing it means re-entering them.

#### Variables read from `.env`

"Required" means production needs a real value. Defaults are those of the compose files (`${VAR:-default}`) or of the example.

**Images and mode**

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `REGISTRY` | yes | `amaze-sms` | Image name prefix (`${REGISTRY}/<service>:${TAG}`); your registry if you push images |
| `TAG` | yes | `release` (HA example), `signoff` (VM example) | Image tag |
| `APP_ENV` | yes | `staging` in `app.yml` when unset; `production` in the HA example | `production` turns off every dev-only behaviour. Services default to `production` when the variable is empty, but `app.yml` passes `staging` if `.env` does not set it. `devseed` refuses `production`. Never use `dev` here: it allows header sign-in, the public dev secrets key and private webhook targets. |
| `PPROF` | no | `false` | `true` adds `/debug/pprof` on the ops ports. Only while investigating. |
| `SEED_BALANCE` | no | `10000000` | Demo account balance for `--profile init run seed` (sign-off and soak only) |

**Addresses (private network)**

| Variable | Required | Default (example) | Meaning |
| --- | --- | --- | --- |
| `KAFKA_1`, `KAFKA_2`, `KAFKA_3` | yes | `10.0.1.11`–`13` | Redpanda brokers; `KAFKA_BROKERS` is built from them |
| `PG_1`, `PG_2`, `PG_3` | yes | `10.0.1.21`–`23` | PostgreSQL + Patroni + etcd |
| `REDIS_1`, `REDIS_2`, `REDIS_3` | yes | `10.0.1.31`–`33` | Redis + Sentinel; `REDIS_1` starts as primary |
| `CH_1`, `CH_2`, `CH_3` | yes | `10.0.1.41`–`43` | ClickHouse + Keeper |
| `APP_1`, `APP_2`, `APP_3` | yes | `10.0.1.51`–`53` | App VMs (HAProxy back ends) |
| `LB_1`, `LB_2` | yes (unless cloud NLB) | `10.0.1.61`, `10.0.1.62` | Load balancers (keepalived unicast peers) |
| `LB_VIP` | yes (unless cloud NLB) | `10.0.1.60` | Virtual IP that customers reach |
| `LB_INTERFACE` | yes (unless cloud NLB) | `eth0` | Interface keepalived puts the VIP on |
| `MON_1` | yes | `10.0.1.71` | Prometheus and Grafana; admin-api reads `http://MON_1:9090` |
| `DATA_1` | yes | `10.0.1.21` | Single-node name used by `deploy/vm` files and `signoff.sh`; point it at `PG_1` |
| `SIM_1` | for tests | `10.0.1.81` | Simulators and load generator |

**What the services connect to**

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `POSTGRES_URL` | yes | single-node URL built from `DATA_1` | All three PG nodes, `target_session_attrs=read-write`, e.g. `postgres://sms:<POSTGRES_PASSWORD>@PG_1:5432,PG_2:5432,PG_3:5432/sms?sslmode=disable&target_session_attrs=read-write&connect_timeout=3` |
| `REDIS_ADDRS` | yes | `DATA_1:6379` | In HA, the three Sentinels: `REDIS_1:26379,REDIS_2:26379,REDIS_3:26379` |
| `REDIS_SENTINEL_MASTER` | yes (HA) | empty | Master name Sentinel watches: `amaze`. Empty means `REDIS_ADDRS` is a plain Redis. |
| `CLICKHOUSE_URL` | yes | `http://CH_1:8123` | All three replicas, comma-separated |
| `SMPP_PROXY_PROTOCOL_FROM` | yes (HA) | empty | Comma-separated addresses or CIDRs of the balancers (`LB_1,LB_2`). smpp-server requires a PROXY v2 header from these and refuses one from anyone else, so per-address limits and IP allow lists see the customer. |

**Secrets** (change all of them)

| Variable | Required | Used by | Meaning |
| --- | --- | --- | --- |
| `POSTGRES_PASSWORD` | yes | Patroni bootstrap (`APP_DB_PASSWORD`), services | Password of the application role `sms`; must match the one in `POSTGRES_URL` |
| `PG_SUPERUSER_PASSWORD` | yes | Patroni | `postgres` superuser (also used for pg_rewind) |
| `PG_REPLICATION_PASSWORD` | yes | Patroni | `replicator` role |
| `PATRONI_REST_PASSWORD` | yes | Patroni, `patronictl` | Patroni REST user `patroni`. Health checks stay open; config, switchover, restart and reinit need it. |
| `ETCD_ROOT_PASSWORD` | yes | etcd, Patroni | etcd `root` user. Authentication is turned on once in step 3.4, including the `grant-role` step. |
| `REDIS_PASSWORD` | yes | Redis, services | Redis `requirepass` and `masterauth` |
| `REDIS_SENTINEL_PASSWORD` | yes (HA) | Sentinel, services | Sentinel `requirepass`. Without it anyone reaching 26379 could point the platform at their own Redis. |
| `CLICKHOUSE_PASSWORD` | yes | ClickHouse, services | ClickHouse user `sms` |
| `SECRETS_KEY` | yes | admin-api, connector, webhook-dispatcher, alerter, devseed | 64 hex characters (AES-256-GCM). Required outside `APP_ENV=dev`. Identical on every VM, and never changed afterwards. |
| `BACKUP_CIPHER_PASS` | yes | pgBackRest | Encrypts both PostgreSQL backup repositories. Keep a copy outside the platform. |
| `LB_VRRP_PASSWORD` | yes (unless cloud NLB) | keepalived | VRRP authentication |
| `LB_STATS_PASSWORD` | yes (unless cloud NLB) | HAProxy | Stats page on `LB_n:7000`, user `admin` |
| `GRAFANA_ADMIN_PASSWORD` | yes | Grafana | User `admin` |

**Backups**

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `S3_ENDPOINT` | yes | `s3.ap-south-1.amazonaws.com` | Host name of the S3-compatible service (no scheme) |
| `S3_PORT` | no | `443` | Port of the S3 service (pgBackRest, Redis and ClickHouse backups) |
| `S3_REGION` | yes | `ap-south-1` | Region |
| `S3_BUCKET` | yes | `amaze-sms-backups` | Bucket |
| `S3_ACCESS_KEY`, `S3_SECRET_KEY` | yes | empty | Key limited to the bucket |
| `S3_URI_STYLE` | no | `host` | `path` for MinIO and most self-hosted stores |
| `S3_VERIFY_TLS` | no | `y` | `n` only for a store with a self-signed certificate (PostgreSQL and Redis backups) |
| `BACKUP_NFS_PATH` | yes | `/mnt/backup` | Where the NFS share is mounted on PG, Redis and CH VMs |
| `BACKUP_RETENTION_FULL_S3` | no | `4` | PostgreSQL weekly fulls kept in S3 |
| `BACKUP_RETENTION_FULL_NFS` | no | `2` | PostgreSQL fulls kept on NFS |
| `BACKUP_KEEP_DAYS` | no | `35` | Days of Redis and ClickHouse copies kept on NFS (S3 copies expire by the bucket's lifecycle rule) |

**Sizing**

| Variable | Default (HA example) | Meaning |
| --- | --- | --- |
| `PG_SHARED_BUFFERS` | `16GB` | PostgreSQL `shared_buffers` |
| `PG_MAX_CONNECTIONS` | `400` | PostgreSQL `max_connections` |
| `REDIS_MAXMEMORY` | `24gb` | Redis `maxmemory` (policy `noeviction`) |
| `REDPANDA_SMP` | `6` | Redpanda cores |
| `REDPANDA_MEMORY` | `24G` | Redpanda memory |

**Public names** (read by `lb.yml`)

| Variable | Required | Default (example) | Meaning |
| --- | --- | --- | --- |
| `API_HOST` | yes | `api.example.com` | Name customers use for the HTTP API (`https://API_HOST/v1/...`) |
| `DLR_HOST` | yes (HTTP vendors) | `dlr.example.com` | Name HTTP vendors post receipts to (`https://DLR_HOST/dlr/<vendor>?token=…`); other paths answer 404 |

Every other name pointed at the balancers (the portal's own name, resellers' domains) goes to the portal.

**Sign-in and portal**

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `OIDC_ISSUER` | yes | empty | Keycloak realm URL, e.g. `https://sso.example.com/realms/amaze`. admin-api refuses to start without it outside `APP_ENV=dev`. |
| `OIDC_AUDIENCE` | no | `amaze-admin,amaze-portal` | Client IDs whose tokens are accepted; admin-api refuses to start with it empty |
| `OIDC_DISCOVERY_URL` | no | empty | Only if admin-api reaches Keycloak at another URL than `OIDC_ISSUER` |
| `PORTAL_OIDC_ISSUER` | no | empty (uses `OIDC_ISSUER`) | Realm URL browsers use, when it differs from `OIDC_ISSUER` |

**Monitoring alerts (Alertmanager on `MON_1`)**

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `ALERT_EMAIL_TO` | yes | `ops@example.com` | Where Prometheus alerts are e-mailed, through the `SMTP_*` relay below |
| `ALERT_WEBHOOK_URL` | no | empty | Also post alerts here (for example a Slack, Teams or PagerDuty bridge) |

**E-mail and customer alerts (alerter, Alertmanager)**

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `SMTP_HOST` | yes (alert e-mails) | empty | SMTP relay. For alerter, empty means e-mail is not configured (recorded on each alert). |
| `SMTP_PORT` | no | `587` | |
| `SMTP_USER`, `SMTP_PASSWORD` | no | empty | |
| `SMTP_FROM` | for e-mail alerts | `alerts@example.com` in the example | Sender |
| `SMTP_TLS` | no | `starttls` | `starttls`, `tls` or `none` (Alertmanager requires STARTTLS only for `starttls`) |
| `ALERT_SMS_URL` | for SMS alerts | empty | The platform's own HTTP API, used as an alerts account |
| `ALERT_SMS_API_KEY` | for SMS alerts | empty | API key of that alerts account |
| `ALERT_SMS_SENDER` | for SMS alerts | empty | Sender ID (a DLT header registered for the alerts account) |

**HLR (router)** — leave `HLR_URL` empty to run without HLR. See [phase3-network.md](phase3-network.md).

| Variable | Meaning |
| --- | --- |
| `HLR_URL` | Provider URL with `{msisdn}` |
| `HLR_AUTH_HEADER`, `HLR_AUTH_VALUE` | Authentication header (example default `Authorization`) |
| `HLR_MCCMNC_PATH`, or `HLR_MCC_PATH` and `HLR_MNC_PATH` | Where the network is in the answer |
| `HLR_STATUS_PATH` | Where the status is in the answer |
| `HLR_STATUS_OK`, `HLR_STATUS_ABSENT`, `HLR_STATUS_INVALID` | Comma-separated status values for each outcome |

**Set on the command line, not in `.env`**

| Variable | Used with | Meaning |
| --- | --- | --- |
| `NODE` | `app.yml` | `app-1`, `app-2`, `app-3`; becomes `NODE_ID` |
| `NODE_NAME`, `SELF` | `postgres.yml` | `pg-1`..`pg-3` and this VM's IP |
| `SELF` | `redis.yml` | this VM's IP |
| `NODE_NAME`, `KEEPER_ID`, `SELF` | `clickhouse.yml` | `ch-1`..`ch-3`, `1`..`3`, this VM's IP |
| `BROKER`, `SELF` | `kafka.yml` | `1`..`3`, this VM's IP |
| `SELF`, `LB_PRIORITY` | `lb.yml` | this VM's IP; `150` on LB_1, `100` on LB_2 |

#### Service settings not wired into `app.yml`

The services read more settings than `app.yml` passes. Their defaults suit production; to change one you have to add it to the service's `environment` in `app.yml`, because a value in `.env` alone does not reach the container.

| Variable | Service | Default | Meaning |
| --- | --- | --- | --- |
| `LOG_LEVEL` | all | `info` (fixed in `app.yml`) | |
| `OPS_ADDR` | all | `:9090` (set per service in `app.yml`) | Ops endpoints |
| `SERVICE_NAME` | all | the service name | Log field |
| `PORTAL_OIDC_CLIENT_ID` | admin-api | `amaze-portal` | Public client the portal signs in with |
| `PORTAL_DIR` | admin-api | `/portal` (set in the image) | Portal static export |
| `ADMIN_ADDR` | admin-api | `:8081` | |
| `CLICKHOUSE_DB`, `CLICKHOUSE_USER` | admin-api, ch-writer, billing-check | `sms`, `sms` | |
| `CLICKHOUSE_ENSURE_SCHEMA` | ch-writer | `true` | Create the ClickHouse tables at start |
| `SMPP_ADDR` | smpp-server | `:2775` | |
| `SMPP_SYSTEM_ID` | smpp-server | `AMAZE` | system_id in bind responses |
| `SMPP_TLS_CERT`, `SMPP_TLS_KEY` | smpp-server | empty | Native SMPP over TLS (the HA layout terminates TLS on the balancer instead) |
| `SMPP_MAX_CONNS_PER_IP` | smpp-server | `64` | Connections per client address |
| `SMPP_BIND_TIMEOUT` | smpp-server | `10s` | A connection not bound by then is closed |
| `SMPP_MAX_BIND_FAILS` | smpp-server | `10` | Failed binds a minute from one address before it is refused for 5 minutes |
| `DLT_TLV_PE_ID`, `DLT_TLV_TEMPLATE_ID`, `DLT_TLV_TM_CHAIN` | smpp-server | `0x1400`, `0x1401`, `0x1402` | TLV tags customers use for DLT fields |
| `DLT_REQUIRE_TM_CHAIN` | smpp-server, http-api | `true` | Refuse messages with no TM chain (registered or sent) |
| `HTTP_ADDR` | http-api | `:8080` | |
| `HTTP_ACCOUNT_TPS` | http-api | `200` | Per-account HTTP rate |
| `HLR_TIMEOUT`, `HLR_CACHE_TTL`, `HLR_CONCURRENCY` | router | `2s`, `48h`, `64` | |
| `DLR_TIMEOUT` | dlr-processor | `72h` | Messages with no receipt by then expire (must stay below 96 h) |
| `RECONCILE_EVERY` | dlr-processor | `1m` | |
| `VENDOR_ID` | connector | `*` (set in `app.yml`) | `*` runs every vendor that is not disabled and has an enabled bind or an HTTP profile; `<id>` runs that one vendor only |
| `VENDOR_RESCAN` | connector | `30s` | How often the vendor list is read: new vendors start, changed ones restart, disabled ones stop |
| `ROUTE_TOPIC_PARTITIONS` | connector | `24` | Partitions of a vendor's `sms.route.<vendor>` topic when the connector creates it |
| `ROUTE_TOPIC_REPLICAS` | connector | `-1` (`3` in `app.yml`) | Replicas of that topic; `-1` is the broker's default. Above 1, `min.insync.replicas` is set to 2 |
| `DLR_ADDR` | connector | `:8090` (set in `app.yml`) | HTTP vendors' receipt listener (`/dlr/<vendor>`) |
| `WEBHOOK_WORKERS` | webhook-dispatcher | `128` | |
| `WEBHOOK_ALLOW_PRIVATE` | webhook-dispatcher, alerter | `false` | Dev only: allow webhooks to private addresses. Keep `false`. |
| `ALERT_EVERY` | alerter | `30s` | Check interval |
| `ALERT_SMS_TEMPLATE` | alerter | `Low balance alert for {name}: {available} available, alert level {threshold}. Please add funds.` | Must match the alerts account's DLT template |

### 3.3 Build or pull the images

On each VM that runs a given file, or once and push to `REGISTRY`:

```sh
cd /opt/amaze/deploy/vm
docker compose -f app.yml --env-file .env --profile init --profile ops build
docker compose -f sim.yml --env-file .env build            # only if you run the sign-off, drills or soak
cd /opt/amaze/deploy/ha
docker compose -f postgres.yml --env-file .env build       # PostgreSQL 16 + Patroni 4.0.4 + pgBackRest
```

The other files use public images (`redis:7.2`, `clickhouse/clickhouse-server:24.8`, `redpandadata/redpanda:v24.2.7`, `quay.io/coreos/etcd:v3.5.17`, `haproxy:2.8`, `osixia/keepalived:2.0.20`, `oliver006/redis_exporter:v1.62.0`, `prom/prometheus:v2.54.1`, `prom/alertmanager:v0.27.0`, `grafana/grafana:11.2.0`, `prom/node-exporter:v1.8.2`).

### 3.4 Kafka (Redpanda) on kafka-1..3

```sh
cd /opt/amaze/deploy/vm
BROKER=1 SELF=10.0.1.11 docker compose -f kafka.yml --env-file .env up -d     # on kafka-1
BROKER=2 SELF=10.0.1.12 docker compose -f kafka.yml --env-file .env up -d     # on kafka-2
BROKER=3 SELF=10.0.1.13 docker compose -f kafka.yml --env-file .env up -d     # on kafka-3
```

Topic auto-creation is off (`auto_create_topics_enabled=false`); the platform's topics are created in step 3.8, and each vendor's route topic by the connector.

### 3.5 PostgreSQL: etcd, its authentication, Patroni, pgBackRest

1. Start etcd on all three PG VMs:
   ```sh
   cd /opt/amaze/deploy/ha
   NODE_NAME=pg-1 SELF=10.0.1.21 docker compose -f postgres.yml --env-file .env up -d etcd   # pg-1
   NODE_NAME=pg-2 SELF=10.0.1.22 docker compose -f postgres.yml --env-file .env up -d etcd   # pg-2
   NODE_NAME=pg-3 SELF=10.0.1.23 docker compose -f postgres.yml --env-file .env up -d etcd   # pg-3
   ```
2. **Turn etcd authentication on, once, on pg-1**, before Patroni first starts. The root user must get the root role before `auth enable`, or etcd refuses it:
   ```sh
   NODE_NAME=pg-1 SELF=10.0.1.21 docker compose -f postgres.yml --env-file .env exec etcd sh -c \
     'etcdctl user add root --new-user-password="$ETCD_ROOT_PASSWORD" &&
      etcdctl user grant-role root root && etcdctl auth enable'
   ```
   Check: an unauthenticated read now fails ("user name is empty").
3. Start Patroni on all three (same `NODE_NAME`/`SELF` as above):
   ```sh
   NODE_NAME=pg-1 SELF=10.0.1.21 docker compose -f postgres.yml --env-file .env up -d   # and pg-2, pg-3
   ```
   On a new cluster Patroni's `post_bootstrap` creates the role `sms` (password `POSTGRES_PASSWORD`) and the database `sms`. One replica is synchronous (`synchronous_mode: true`).
4. Find the leader:
   ```sh
   docker compose -f postgres.yml --env-file .env exec patroni patronictl -c /etc/amaze/patroni.yml list
   ```
5. **On the leader**, create the pgBackRest stanza in both repositories and take the first full backups:
   ```sh
   docker compose -f postgres.yml --env-file .env exec patroni /etc/amaze/backup.sh init
   ```
   WAL archiving starts once the stanza exists (`/backup/pgbackrest/stanza.ok`); until then PostgreSQL keeps WAL in `pg_wal`. Check with `backup.sh check` and `backup.sh info`.

### 3.6 Redis and Sentinel on redis-1..3

Start redis-1 first; it begins as primary and the others replicate from it.

```sh
cd /opt/amaze/deploy/ha
SELF=10.0.1.31 docker compose -f redis.yml --env-file .env up -d    # redis-1
SELF=10.0.1.32 docker compose -f redis.yml --env-file .env up -d    # redis-2
SELF=10.0.1.33 docker compose -f redis.yml --env-file .env up -d    # redis-3
```

This also starts `exporter` (redis_exporter, metrics on `SELF:9121` for Prometheus). The configuration is written into the volume on first start only; afterwards Sentinel rewrites it, so a restarted node comes back in its current role. Changing `REDIS_PASSWORD`, `REDIS_SENTINEL_PASSWORD` or `REDIS_MAXMEMORY` later means editing `/data/redis.conf` and `/data/sentinel.conf` in the volumes.

Check (on a Redis VM, with `.env` loaded into the shell):

```sh
set -a; . ./.env; set +a
docker compose -f redis.yml --env-file .env exec sentinel \
  redis-cli -p 26379 -a "$REDIS_SENTINEL_PASSWORD" --no-auth-warning SENTINEL get-master-addr-by-name amaze
```

### 3.7 ClickHouse and Keeper on ch-1..3

```sh
cd /opt/amaze/deploy/ha
NODE_NAME=ch-1 KEEPER_ID=1 SELF=10.0.1.41 docker compose -f clickhouse.yml --env-file .env up -d   # ch-1
NODE_NAME=ch-2 KEEPER_ID=2 SELF=10.0.1.42 docker compose -f clickhouse.yml --env-file .env up -d   # ch-2
NODE_NAME=ch-3 KEEPER_ID=3 SELF=10.0.1.43 docker compose -f clickhouse.yml --env-file .env up -d   # ch-3
```

On first start `clickhouse/init.sh` creates the `sms` database with the `Replicated` engine, so the tables ch-writer creates on one replica appear on all three. Nothing else needs to be created by hand. Each replica serves Prometheus metrics on port 9363 (`clickhouse/cluster.xml`).

### 3.8 Initialise the schema and topics (from app-1)

```sh
cd /opt/amaze/deploy/vm
docker compose -f app.yml --env-file .env --profile init run --rm migrate
docker compose -f app.yml --env-file .env --profile init run --rm topics
```

- **migrate** applies `migrations/postgres/0001`–`0014` in order, each in its own transaction, under an advisory lock (safe to run again; applied versions are in `schema_migrations`).
- **ClickHouse tables** (`migrations/clickhouse`) are created by ch-writer when it starts (`CLICKHOUSE_ENSURE_SCHEMA=true`).
- **topics** runs `deploy/kafka-topics.sh` with 24 partitions and 3 replicas (`min.insync.replicas=2`): `sms.submit`, `sms.reroute`, `sms.status`, `sms.dlr`, `sms.mo`, `sms.webhooks`, `sms.records` (72 h), `sms.billing.events`, `sms.config.changes`, `sms.webhooks.retry.1`–`4`, `sms.webhooks.dead` (7 days), and `sms.route.<vendor>` for `VENDORS: sim httpsim`.
- **Real vendors' topics** (`sms.route.<vendor>`) are created by the connector when it first starts the vendor, with the same settings (24 partitions, 3 replicas, 72 h, zstd, `min.insync.replicas=2`). Nothing to do by hand.

Do **not** run `seed` in production: it creates demo accounts and routes to the simulator, and it refuses `APP_ENV=production` anyway.

### 3.9 Keycloak realm for production

Following [admin-auth.md](admin-auth.md):

1. On your Keycloak server, create the realm `amaze` by importing `deploy/keycloak/amaze-realm.json`. It brings two public clients (`amaze-admin`, `amaze-portal`, PKCE S256), brute-force protection, a password policy (`length(12) and notUsername`), 10-minute access tokens and a 30-minute idle session.
2. Then change it for production:
   - delete the demo user `ops@amaze.local`;
   - turn off `directAccessGrantsEnabled` on `amaze-admin` (it is on for dev scripts);
   - replace the redirect URIs: `amaze-admin` has `http://localhost:3000/*`, and `amaze-portal` has `http://localhost:8081/callback/` and post-logout `http://localhost:8081/login/`;
   - require OTP for the realm (the imported file sets nothing about OTP).
3. On `amaze-portal`, for every portal origin (the platform's portal name and each reseller domain), add `https://<domain>/callback/` as a redirect URI and `https://<domain>/login/` as a post-logout redirect URI, with the matching web origin. Do not use wildcards: a code could then be sent to any site.
4. Create the staff in Keycloak, with **verified** e-mails (admin-api refuses tokens without `email_verified`).
5. Set `OIDC_ISSUER` in `.env` to the realm URL as it appears in tokens (`iss`). If browsers reach Keycloak at another URL, set `PORTAL_OIDC_ISSUER`. The portal's Content-Security-Policy allows only the portal and the sign-in server.

### 3.10 Platform account and first platform admin

1. **Create the platform account and grant its first owner** in one step, from one app VM (the e-mail exactly as in Keycloak):
   ```sh
   cd /opt/amaze/deploy/vm
   docker compose -f app.yml --env-file .env --profile ops run --rm admin-user \
     -create-platform "Your Company" -email owner@yourcompany.com -role owner
   ```
   `-create-platform` creates the account with role `platform` only if none exists; run again, it prints `a platform account exists already; left as it is` and just grants the user. `migrate` does not create the platform account, and `seed` must not be run in production.
2. **More platform staff** the same way without `-create-platform`: `... run --rm admin-user -email someone@yourcompany.com -role admin`. Roles: `owner`, `admin`, `finance`, `support`, `viewer`. `-account <id>` grants on another account.
3. Everyone else is granted from the portal (Team page) or `POST /v1/accounts/{id}/users {"email":"…","role":"finance"}`.

### 3.11 Platform services on app-1..3

```sh
cd /opt/amaze/deploy/vm
NODE=app-1 docker compose -f app.yml --env-file .env up -d     # app-1
NODE=app-2 docker compose -f app.yml --env-file .env up -d     # app-2
NODE=app-3 docker compose -f app.yml --env-file .env up -d     # app-3
```

This starts smpp-server, http-api, admin-api, router, rater, dlr-processor, connector, webhook-dispatcher, ch-writer and alerter. admin-api starts by default and needs `OIDC_ISSUER` (step 3.9); without it, it exits and the balancer's portal health check on 9103 fails.

The one `connector` service runs every vendor that is not disabled and has an enabled bind or an HTTP profile. Vendors added in the portal start within 30 seconds (`VENDOR_RESCAN`), a vendor whose binds or HTTP profile change is restarted with the new settings, and a disabled vendor is stopped. There are no per-vendor services to add (step 3.15).

Check each service:

```sh
for p in 9101 9102 9103 9104 9105 9106 9107 9109 9110 9111; do
  printf '%s %s\n' $p "$(curl -s -o /dev/null -w '%{http_code}' http://10.0.1.51:$p/readyz)"
done
```

Every line should say 200. A 503 body names the dependency that failed.

### 3.12 Load balancers on lb-1 and lb-2

1. Put `smpp.pem` (chain, then key) in `deploy/ha/lb/certs/` on both.
2. Put one PEM per HTTPS name (chain, then key) in `deploy/ha/lb/certs/https/` on both: the portal's name, `API_HOST`, `DLR_HOST` and every reseller domain. HAProxy picks the certificate by the name the client asks for (SNI), and refuses to start while this folder has none.
3. Check that `API_HOST` and `DLR_HOST` are set in `.env`, and that DNS for every name points at `LB_VIP`'s public address.
4. Start them:
   ```sh
   cd /opt/amaze/deploy/ha
   SELF=10.0.1.61 LB_PRIORITY=150 docker compose -f lb.yml --env-file .env up -d   # lb-1
   SELF=10.0.1.62 LB_PRIORITY=100 docker compose -f lb.yml --env-file .env up -d   # lb-2
   ```
5. What HAProxy does (`lb/haproxy.cfg`):
   - **SMPP** `:2775` and `:3550` (TLS) → app `:2775`, TCP, least connections, `send-proxy-v2`, health from `/readyz` on 9101;
   - **HTTPS** `:443` (port 80 only redirects to HTTPS), routed by host name:
     - `API_HOST` → HTTP API, app `:8080`, health on 9102;
     - `DLR_HOST` → connector receipts, app `:8090`, health on 9107; only paths under `/dlr/` pass, anything else gets 404;
     - every other name (the portal's name, resellers' domains) → portal and admin API, app `:8081`, health on 9103;
     - it sets `X-Forwarded-For` to the client address (replacing what the client sent; admin-api records it in the audit log) and `X-Forwarded-Proto: https` (admin-api then sends HSTS);
   - **plain HTTP API and portal** on `LB:8080` and `LB:8081`, private address only, for operators and tools;
   - **PostgreSQL** primary `LB:5000` and replicas `LB:5001` on the private address, health from Patroni (`OPTIONS /primary`, `/replica` on 8008);
   - **stats** on `LB:7000` and **Prometheus metrics** on `LB:7001/metrics`, private address only.
   An app VM is taken out after 3 failed checks 2 s apart, and its sessions are shut down so SMPP binds reconnect elsewhere.
6. **Adding or renewing a certificate** (for example a new reseller domain): put its PEM in `certs/https/` on both LB VMs and reload HAProxy on each:
   ```sh
   cd /opt/amaze/deploy/ha && docker compose -f lb.yml --env-file .env kill -s HUP haproxy
   ```
7. On a public cloud, skip this and use the cloud's balancer. It has to terminate HTTPS and route by host name as above, and pass SMPP as TCP with PROXY v2 (smpp-server then needs that balancer's addresses in `SMPP_PROXY_PROTOCOL_FROM`).

### 3.13 Monitoring on mon-1

```sh
cd /opt/amaze
scripts/signoff.sh render-monitoring        # writes prometheus.yml and alertmanager.yml in deploy/vm/monitoring/ from deploy/vm/.env
cd deploy/vm && docker compose -f monitoring.yml --env-file .env up -d
```

Run `render-monitoring` again, and restart Prometheus and Alertmanager, after changing the VMs, `SMTP_*` or `ALERT_*` in `.env`. `alertmanager.yml` holds the SMTP password and is written readable by its owner only.

- **Grafana:** `http://MON_1:3000`, user `admin` / `GRAFANA_ADMIN_PASSWORD`, dashboard **SMS sign-off** (accepted vs final, in flight, accept latency, refusals by reason, vendor submits and latency, DLRs, final status, consumer throughput and batch time per group, host and service CPU, webhooks, binds).
- **Prometheus:** keeps 30 days, scrapes every 5 s.
- **Targets:** `render-monitoring` scrapes every VM named in `.env`, for either layout (sign-off or HA):
  - app ops ports 9101–9107 and 9109–9111 on `APP_1`, `APP_2` and `APP_3`;
  - Redpanda on the three Kafka VMs (9644);
  - node-exporter (9100) on every VM;
  - HA layout only: Patroni on the PG VMs (8008), redis_exporter on the Redis VMs (9121), ClickHouse on the CH VMs (9363) and HAProxy on `LB_1`/`LB_2` (7001).
  The firewall must let `MON_1` reach all of these ([network table](#network-and-firewall)).
- **Alerts:** Prometheus evaluates the rules in `deploy/vm/monitoring/alerts.yml` (25 rules) and Alertmanager (`http://MON_1:9093`) e-mails them to `ALERT_EMAIL_TO` through the `SMTP_*` relay, and also posts them to `ALERT_WEBHOOK_URL` if set. Resolved alerts are sent too. `critical` (act now) repeats every hour, `warning` (look today) every 4 hours.

  | Group | Main alerts |
  | --- | --- |
  | availability | a scrape target down 2 min; messages accepted but none reach a vendor; submits refused as `unavailable`; p99 accept time over 250 ms; a consumer retrying batches; history rows dropped; customer webhooks given up |
  | vendors | a vendor with no SMPP bind up; out of routing 10 min (failover); over 20% of submits failing; under 50% of receipts `DELIVRD` |
  | data | PostgreSQL not exactly one primary, or down; Redis not exactly one primary, fewer than 2 replicas, over 85% of maxmemory; ClickHouse read-only replicas; Kafka partitions without a leader or replicas behind |
  | balancers | a HAProxy back end with no healthy server; a server out |
  | machines | disk under 15% / 5% free, memory under 10% available, CPU over 90% busy |

- **Backups are not alerted on.** Check them yourself, or add a cron check: PostgreSQL `pg_stat_archiver.failed_count` rising, `backup.sh info` showing a newest backup older than 8 days, and errors in ClickHouse `system.backups` (phase5-ha.md, "Setting up backups").

### 3.14 Backups schedule

Install `deploy/ha/backup.cron` with `crontab -e`, as a user that can run docker, on the VMs each block names (it assumes `/opt/amaze`):

| Where | When | What |
| --- | --- | --- |
| all PG VMs (only the leader acts) | 01:15 Sunday full, other days diff | `backup.sh full` / `diff` to S3, then NFS, then `expire` |
| CH_1 only | 02:45 Sunday full, other days incremental | `backup.sh full` / `incr` to S3 and the NFS disk |
| all Redis VMs (only a replica acts) | every hour at :05 | `deploy/ha/redis-backup.sh`: RDB snapshot to NFS and S3 |

Output goes to `/var/log/amaze-backup.log`. Run the ClickHouse full once by hand now, because `incr` refuses to run before a full exists:

```sh
cd /opt/amaze/deploy/ha && docker compose -f clickhouse.yml --env-file .env exec -T clickhouse backup.sh full
```

### 3.15 Initial configuration

Sign in to the portal as the platform owner and set up, in this order (pages under **Operations** are platform-only; [phase4-portals.md](phase4-portals.md) has the details):

1. **Branding** for the platform account (what direct customers see).
2. **Rate cards:** a platform card with prices by destination and category. A customer cannot send without a rate card; new accounts inherit their parent's.
3. **Number data:** import number series (`prefix,mcc,mnc[,operator,circle]`) and the MNP dump (`msisdn,mcc,mnc`; import it daily). Edges reload series every 5 minutes.
4. **Error codes:** vendor receipt codes mapped to a reason and customer code.
5. **Vendors:** for each vendor:
   1. **Vendors → New vendor** (starts paused); add SMPP binds (with DLT TLV tags) or the HTTP profile; import costs.
   2. **Nothing to deploy.** Within 30 seconds the connector on every app VM starts the vendor, creating its `sms.route.<vendor>` topic on the way. Bind or profile edits restart it there within 30 seconds (the vendor reconnects); disabling the vendor stops it.
   3. **Bind count:** every app VM opens every enabled bind. With three app VMs, a vendor that allows 6 binds gets 2 bind rows in the portal.
   4. HTTP vendors post receipts to `https://<DLR_HOST>/dlr/<vendor>?token=…` (the portal shows the path, with its token, to admins). Give the vendor that URL.
   5. Optionally set a failover policy, then **activate** the vendor.
6. **Routes:** default and per-account (least cost, priority, weighted; HLR options).
7. **Firewall:** block lists and pumping-guard limits.
8. **Simulator vendors:** in production, `sim` and `httpsim` have no binds or profile unless seeded, so the connector does not run them. If you seeded for the sign-off, disable them afterwards.
9. **Alerts account** (for SMS alerts): a customer account with an API key, a DLT PE ID, header and a template matching `ALERT_SMS_TEMPLATE`, e.g. `Low balance alert for {#var#}: {#var#} available, alert level {#var#}. Please add funds.`. Put its key and sender in `ALERT_SMS_API_KEY` / `ALERT_SMS_SENDER`, with `ALERT_SMS_URL` pointing at the HTTP API, and restart alerter.
10. **Resellers and customers:** create accounts, top up, set credit limits (default 0), DLT registrations (PE IDs, headers, templates, TM chain), SMPP credentials and API keys. For a reseller domain: set it on the reseller's branding (platform only), add its certificate to `certs/https/` on both balancers and reload HAProxy (step 3.12), and add its redirect URIs in Keycloak (step 3.9).

---

## 4. Verify the installation

### 4.1 Health

1. Every ops port on every app VM answers 200 on `/readyz` (step 3.11).
2. HAProxy stats (`http://LB_1:7000`, user `admin`) show every back end up (`http-api`, `portal`, `vendor-dlr`, `smpp`, the PostgreSQL ones).
3. `patronictl list` shows one leader, one synchronous standby, one replica.
4. Sentinel names the primary; `INFO replication` on it shows two replicas.
5. On a CH replica, `SELECT * FROM system.replicas` shows the `sms` tables healthy (after ch-writer started).
6. `backup.sh check` passes on the PG leader; `backup.sh list` on CH_1 shows the first full.
7. The portal loads at `https://<portal>`, sign-in goes through Keycloak, and `GET /v1/me` returns the owner:
   ```sh
   curl -s https://<portal>/v1/me -H "Authorization: Bearer <token>"
   ```
8. HTTPS by name: `http://<portal>` redirects to HTTPS; `https://<API_HOST>/` reaches the HTTP API (send a test message as in [api-reference.md](api-reference.md)); `https://<DLR_HOST>/` answers 404 (only `/dlr/<vendor>` passes). Each name shows its own certificate.
9. Prometheus (`http://MON_1:9090/targets`) shows every target up, and Alertmanager (`http://MON_1:9093`) is reachable. To test e-mail, stop node-exporter on one VM for 3 minutes: `TargetDown` reaches `ALERT_EMAIL_TO`, then a resolved message when it is back.

### 4.2 Sign-off stages on the production VMs (before go-live)

The 5,000 TPS sign-off runs on the HA layout as well ([signoff-5k.md](signoff-5k.md)): the services take the same settings. It needs the simulators and the seed data, so run it **before** real customers are on the platform.

1. On sim-1: `docker compose -f deploy/vm/sim.yml --env-file deploy/vm/.env up -d smpp-sim http-sim`.
2. Seed the demo account. The seed refuses `APP_ENV=production`, so run it as staging:
   ```sh
   cd /opt/amaze/deploy/vm
   docker compose -f app.yml --env-file .env --profile init run --rm -e APP_ENV=staging seed
   ```
   This writes demo accounts, the `demo` SMPP credential (password `demo123`) and routes to the simulator into the production database. Decide beforehand whether you will rebuild the data tier after the sign-off or suspend the demo accounts and remove their routes; the repository has no clean-up tool.
3. On sim-1: `scripts/signoff.sh run`. It runs three gated stages (reports in `deploy/vm/reports/<stage>.json`). The load generator binds to `APP_1` and `APP_2` directly, not through the balancer.

   | Stage | Load | Pass when |
   | --- | --- | --- |
   | smoke | 500 TPS, 2 min | 0 refused, ≥ 99% accepted, every receipt within 30 s |
   | steady | 5,000 TPS, 30 min | 0 refused, ≥ 99% accepted, p99 ≤ 100 ms (`MAX_P99`), every receipt, last ≤ 60 s after sending stops (`MAX_LAG`) |
   | burst | 10,000 TPS, 5 min | 0 refused, ≥ 98% accepted, p99 ≤ 250 ms (`BURST_MAX_P99`), every receipt within 5 min |

4. On an app VM, with the start time the script printed: `scripts/signoff.sh billing <since>`. It passes when no holds are open, Redis and PostgreSQL balances agree, ledger charges equal ClickHouse per account, and every message has a final status.

### 4.3 Failover drills

Run them during a second steady run (`STEADY_DURATION=20m scripts/signoff.sh run`) and run `billing` afterwards. Expect a few duplicate vendor submits after kill drills, but no lost message and an exact billing check.

From [signoff-5k.md](signoff-5k.md) (application tier):

| Drill | Command | Expected |
| --- | --- | --- |
| Kill a connector | on app-1: `docker kill amaze-app-connector-1` | another VM's connector takes the partitions in about 10 s; restart policy brings it back |
| Kill a Kafka broker | on kafka-2: `docker kill amaze-kafka-redpanda-1`, start it after 2 min | no refused submits; brief latency spike |
| Kill an SMPP edge | on app-2: `docker kill amaze-app-smpp-server-1` | the load generator's binds on app-2 drop (it connects to the app VMs directly) and those on app-1 keep sending; judge this run on billing and lost messages only |
| Vendor outage | on sim-1: `docker stop amaze-sim-smpp-sim-1`, start after 60 s | `sms_vendor_routable{vendor="sim"} 0` within about 3 s, traffic moves to `httpsim` |

From [phase5-ha.md](phase5-ha.md) (data tier; `scripts/ha-drill.sh` does these on the local rehearsal tier only):

| Drill | Command | Expected (measured in rehearsal) |
| --- | --- | --- |
| PostgreSQL leader crash | on the leader: `docker kill amaze-pg-patroni-1` | new leader in 18–20 s; no submit refused; old leader returns as a replica via pg_rewind |
| Redis primary crash | on the primary: `docker kill amaze-redis-redis-1` | Sentinel promotes in about 4.5 s; submits refused for those seconds with a temporary error (SMPP `ESME_RMSGQFUL`, HTTP 503); run `wallet-resync` afterwards |
| ClickHouse replica crash | on one CH VM: `docker kill amaze-clickhouse-clickhouse-1` | writer moves to the next replica at once; replica catches up when back |
| Keeper quorum lost | stop ClickHouse on two CH VMs for 30 s | history waits in Kafka; writable again within about 40 s of restart |
| App VM lost | stop all containers on one app VM | balancer drops it within about 6 s |
| LB lost | stop keepalived/haproxy on the LB holding the VIP | VIP moves to the other LB |

Check Patroni with `patronictl list`, Sentinel with `SENTINEL get-master-addr-by-name amaze`, and run `billing` after each run.

### 4.4 Restore check

`scripts/ha-restore-check.sh` proves the restores on the local rehearsal tier only; there is no production script. On production, test restores into scratch locations at least once before go-live, following [5.1](#51-backups-and-restore): a ClickHouse table restored under a new name (`RESTORE TABLE sms.message_records AS sms.message_records_restored FROM ...`), a Redis snapshot loaded into a scratch Redis, and a PostgreSQL point-in-time restore on a spare VM.

---

## 5. Day-2 operations

### 5.1 Backups and restore

What is backed up (from [phase5-ha.md](phase5-ha.md)): each store goes to both S3 and NFS, and either alone can restore the platform.

| Store | Tool | Restore point |
| --- | --- | --- |
| PostgreSQL | pgBackRest, two encrypted repositories (repo1 S3, 4 fulls; repo2 NFS, 2 fulls); WAL archived continuously | any moment since the oldest full |
| ClickHouse | `BACKUP DATABASE sms` to `S3(.../clickhouse/<time>-full|incr)` and `Disk('backups', ...)` | the backup time |
| Redis | RDB snapshot from a replica, hourly | the snapshot, then `wallet-resync` |
| Kafka | not backed up: three copies, 72 h retention; everything ends up in PostgreSQL or ClickHouse | — |

Status:

```sh
cd /opt/amaze/deploy/ha
docker compose -f postgres.yml --env-file .env exec patroni /etc/amaze/backup.sh info     # also: check
docker compose -f clickhouse.yml --env-file .env exec clickhouse backup.sh list
ls -l /mnt/backup/redis
```

**PostgreSQL to a point in time** (for example just before a bad change at 14:02):
1. Stop Patroni on all three: `docker compose -f postgres.yml --env-file .env stop patroni`.
2. Remove the cluster from etcd: `patronictl -c /etc/amaze/patroni.yml remove amaze`. The docs give only this line; `patroni.yml` exists only inside a container started by the entrypoint, so run it from a container that renders the configuration first, as step 3 does for pgBackRest.
3. On pg-1, restore from S3 (`--repo=2` for NFS):
   ```sh
   docker compose -f postgres.yml --env-file .env run --rm --entrypoint bash patroni -c \
     'envsubst </etc/amaze/pgbackrest.conf.tmpl >/tmp/pgbr.conf && pgbackrest --config=/tmp/pgbr.conf --stanza=amaze --repo=1 --delta \
      --type=time "--target=2026-10-21 14:01:59+05:30" --target-action=promote restore'
   ```
4. Start Patroni on pg-1, then pg-2 and pg-3: they clone it.
5. Run `wallet-resync` (5.3).

**ClickHouse:** drop the damaged database on all replicas, then on one:

```sql
RESTORE DATABASE sms FROM S3('https://<bucket>.<endpoint>/clickhouse/<name>', '<key>', '<secret>')
-- or FROM Disk('backups', '<name>')
```

Or restore one table under a new name and copy rows back. Messages after the backup come back from Kafka if the ch-writer group is reset to the backup time; beyond 72 h they are gone from history (money is in PostgreSQL).

**Redis** (only if all three lost their data): stop the three Redis containers; copy the newest `redis-*.rdb` into the primary's volume as `dump.rdb`, with `appendonly no` in its `redis.conf` for the first start; start it, then the replicas; run `wallet-resync`. Receipts for messages sent after the snapshot will not match.

### 5.2 Failover

Failovers are automatic (see the table in phase5-ha.md, "What a failure costs"). After any **Redis** failover or restore, and after any PostgreSQL restore, run `wallet-resync`. To move the PostgreSQL leader on purpose (maintenance), use Patroni's switchover from inside the container: `patronictl -c /etc/amaze/patroni.yml switchover` (it uses `PATRONI_REST_PASSWORD` from the rendered configuration).

### 5.3 wallet-resync

Rebuilds every wallet's balance, credit limit and holds in Redis from the PostgreSQL ledger (the system of record), and drops holds of messages sent long ago. It is safe while traffic flows (it takes an advisory lock that every two-store balance change also holds).

```sh
cd /opt/amaze/deploy/vm
docker compose -f app.yml --env-file .env --profile ops run --rm wallet-resync -dry-run   # report
docker compose -f app.yml --env-file .env --profile ops run --rm wallet-resync            # repair
```

Flags: `-stale-after` (default `1h`), `-timeout` (default `10m`).

### 5.4 Upgrades

There is no written upgrade procedure; this is the order the files allow.

1. **Get the code to the VMs.** Development happens on the Windows machine. A Claude session that cannot push hands over a git bundle, saved as `D:\ZAPIM\SMPP\amazeA2pSMS.bundle`; apply it there:
   ```powershell
   powershell -ExecutionPolicy Bypass -File D:\ZAPIM\SMPP\amazeA2pSMS\scripts\sync-from-bundle.ps1
   ```
   The script verifies the bundle, fast-forwards `main` in `D:\ZAPIM\SMPP\amazeA2pSMS` and pushes to `origin` (`-NoPush` skips the push; `-RepoPath` / `-BundlePath` change the paths). It stops if the working tree has uncommitted changes. Wait for CI (`.github/workflows/ci.yml`) to pass on `main`.
2. **On every VM:** `cd /opt/amaze && git pull --ff-only`.
3. **Build** new images with a new `TAG` (or build and push once), as in step 3.3, and set `TAG` in both `.env` files on every VM.
4. **Migrate** from one app VM: `docker compose -f app.yml --env-file .env --profile init run --rm migrate`. Check the new migration files first; the existing ones only add tables and columns, but each release should be read.
5. **New topics:** if `deploy/kafka-topics.sh` gained topics, run `topics` again (existing ones are skipped). Vendors' route topics are the connector's job.
6. **Roll the app VMs one at a time**, waiting for every `/readyz` to answer 200 and HAProxy to mark the VM up before the next:
   ```sh
   NODE=app-1 docker compose -f app.yml --env-file .env up -d
   ```
   SMPP binds on the restarted VM drop and reconnect through the balancer; nothing acknowledged is lost (it is in Kafka first).
7. **Data-tier changes** (`deploy/ha/*`): one node at a time, replicas before the primary/leader.

### 5.5 Logs

| What | Where |
| --- | --- |
| Platform services | container stdout (JSON): `docker compose -f app.yml --env-file .env logs -f <service>` on each app VM |
| Patroni / PostgreSQL | `docker compose -f postgres.yml --env-file .env logs patroni`; pgBackRest logs at `/var/log/pgbackrest` inside the container |
| Redis, Sentinel, ClickHouse, Redpanda, HAProxy, keepalived | `docker compose -f <file> logs <service>` (HAProxy logs to stdout) |
| Backups | `/var/log/amaze-backup.log` on each VM with the cron |
| Admin changes | PostgreSQL table `admin_audit` (every non-GET request: e-mail, account, path, result, source IP) |
| Sign-off and soak reports | `deploy/vm/reports/` on sim-1 (and `soak-report.json` on the app VM that ran it) |

No log rotation or shipping is configured in the compose files; set Docker's log driver options on each VM.

### 5.6 Scaling

- **App tier:** add an app VM (`APP_4` in `.env`, a `server` line in each app `listen` and `backend` block of `haproxy.cfg`, a Prometheus target: `render-monitoring` knows `APP_1`–`APP_3` only), start `app.yml` on it. Kafka spreads partitions over the consumers (24 partitions per topic, so at most 24 consumers per group). Every app VM opens every vendor bind, so check each vendor's allowed binds first and reduce bind rows if needed.
- **One busy service:** the dashboard's "Consumer throughput by group", "Service CPU" and "Host CPU busy" show whether a service is short of CPU (add a VM, or move it to its own) or waiting on Redis or Kafka.
- **Vendors:** one connector process per app VM runs every vendor; vendor binds and TPS are set per vendor in the portal, and every app VM opens every bind (divide a vendor's allowed binds by the number of app VMs).
- **Data stores:** sizing knobs in `.env` (`PG_*`, `REDIS_MAXMEMORY`, `REDPANDA_*`); ClickHouse disk grows about 45 GB a day at 5,000 TPS.

### 5.7 Soak test

The 10,000 TPS, 24-hour soak ([soak-test.md](soak-test.md)) runs on the HA layout, with monitoring up and sim-1 at 16 vCPU. Seed enough balance first (`SEED_BALANCE=150000000` in `.env`, then `seed` as in 4.2), or the run stops with `insufficient_balance`.

```sh
nohup scripts/signoff.sh soak > deploy/vm/reports/soak.log 2>&1 &     # on sim-1
scripts/signoff.sh soak-report <since>                                # on an app VM, afterwards
scripts/signoff.sh billing <since>
```

It fails on any refusal, accepted TPS under 99%, an hourly p99 over 250 ms (`SOAK_MAX_P99`), more than 200,000 receipts outstanding at a checkpoint (`SOAK_MAX_OUTSTANDING`), or receipts missing 15 minutes after sending stops. `soak-report` then fails on memory, goroutine or in-flight growth, restarts, refusals or dropped history rows.

### 5.8 Security checklist

From [phase5-security.md](phase5-security.md):

- [ ] `APP_ENV=production` in `.env` on every VM (never `dev`; `app.yml` falls back to `staging` if unset).
- [ ] `SECRETS_KEY` set, 64 hex characters, the same everywhere, copied somewhere safe.
- [ ] Every `change-me` replaced; `BACKUP_CIPHER_PASS` kept outside the platform.
- [ ] etcd authentication on (unauthenticated reads fail), Patroni REST password set (unauthenticated `PATCH /config` gets 401).
- [ ] Sentinel password set (unauthenticated commands get `NOAUTH`); Redis password set.
- [ ] Firewall: etcd 2379/2380 reachable only from PG and LB VMs, Patroni 8008 only from PG and LB VMs and `MON_1`; data-store, exporter and ops ports only from the private network; only 80, 443, 2775 and 3550 public on the balancers.
- [ ] `SMPP_PROXY_PROTOCOL_FROM` lists exactly the balancers.
- [ ] `PPROF` off.
- [ ] Keycloak: demo user deleted, direct grants off, real redirect URIs (no wildcards), OTP required, staff e-mails verified.
- [ ] `WEBHOOK_ALLOW_PRIVATE` not set.
- [ ] A certificate in `deploy/ha/lb/certs/https/` for every HTTPS name, and renewals scheduled (HAProxy reload, step 3.12).
- [ ] Reseller domains set only by the platform, each with a certificate and Keycloak redirect URIs.
- [ ] CI `security` job (govulncheck, staticcheck, `npm audit`) green on the release commit.
- [ ] Known open items accepted: plaintext traffic inside the private network, PE ID first-come registration, e-mail enumeration on user add, API key revocation takes up to 60 s. (The audit log takes `X-Forwarded-For` as given; HAProxy overwrites that header, so keep admin-api reachable only through the balancer.)

---

## 6. Single-machine trial and troubleshooting

### 6.1 Trial with Docker

Everything on one machine, Go not needed:

```sh
make up                                                 # postgres, redis, redpanda, clickhouse, keycloak, simulator, seed, all services
docker compose -f deploy/docker-compose.yml logs seed   # prints the demo HTTP API key
make down
```

Keycloak is on `localhost:8180` (sign in as `ops@amaze.local` / `dev-only-change-me`), Mailpit on `localhost:8025` for alert e-mails.

### 6.2 Trial without Docker (`scripts/dev-local.sh`)

Needs Linux, WSL or macOS, Go 1.26+, PostgreSQL with database `sms` owned by `sms`/`sms`, and Redis on 6379 (start it outside the repository: `cd /tmp && redis-server --daemonize yes`). **`up` drops the `sms` schema and runs `FLUSHALL` on Redis: never point it at anything that matters.**

```sh
cd portal && npm ci && npm run build && cd ..     # optional: the portal on :8081
scripts/dev-local.sh up                           # build, in-memory Kafka, simulators, all services, demo data
TPS=2000 scripts/dev-local.sh load                # 30 s SMPP load
scripts/dev-local.sh down
```

- **Options:** `CHDB_PYTHON=<python with chdb>` adds a ClickHouse stand-in and ch-writer; `CLICKHOUSE_URL=...` uses a real one; `ROUTE=httpsim` sends demo traffic through the HTTP simulator; `PROMETHEUS_BIN=...` starts Prometheus for the live monitor; `SEED_ARGS="-balance 100000000"` for longer runs.
- **Ports:** SMPP 2775, HTTP 8080, admin API and portal 8081, HTTP-vendor receipts 8090, simulators 2776/2777, ops 9101–9107 and 9109–9111 (one connector on 9107 runs every vendor; vendors added in the portal start within 5 s here). Logs in `.dev/logs/`.
- **Sign-in:** `APP_ENV=dev` without `OIDC_ISSUER`, so the portal lists the seeded users: `ops@amaze.local` (platform), `owner@demo-reseller.local` (reseller), `owner@reseller-shop.local` (its sub-account), `owner@demo-customer.local` (direct customer). Scripts send `X-Actor-Account` / `X-Actor-User` headers instead of a token.
- **Sending:** SMPP `system_id=demo`, `password=demo123`. Only the registered DLT templates pass (PE `1101234567890123456`, header `AMZTST`, `Your OTP is {#var#}. Do not share it with anyone. -Amaze`); other text to the demo customer is refused `dlt_template`.
- **HA rehearsal:** `scripts/ha-local.sh up`, then `eval "$(scripts/ha-local.sh env)"; scripts/dev-local.sh up`, then `scripts/ha-drill.sh all` and `scripts/ha-restore-check.sh`. The tools it needs are listed at the top of `scripts/ha-local.sh`.
- **Stopping processes:** by PID (`.dev/pids`), never `pkill -f`.

### 6.3 Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| A service exits with `secrets: SECRETS_KEY is required outside APP_ENV=dev` | `SECRETS_KEY` missing in `.env`. |
| `secrets: key must be 64 hex characters` | Generate with `openssl rand -hex 32`. |
| admin-api exits with `OIDC_ISSUER is required outside APP_ENV=dev` | Add `OIDC_ISSUER` to `.env` (step 3.9). |
| admin-api logs `OIDC issuer not reachable yet; retrying` | Keycloak unreachable from the app VM, or a wrong realm URL. Set `OIDC_DISCOVERY_URL` in `.env` if admin-api must fetch discovery from another address. |
| Portal sign-in: 401 | Wrong or expired token, issuer mismatch (`iss` must equal `OIDC_ISSUER`), or the client is not in `OIDC_AUDIENCE`. |
| Portal sign-in: 403 | Valid token, but no enabled user for that verified e-mail on an active account: grant one with `admin-user` or the Team page. |
| Keycloak: invalid redirect URI | Add `https://<domain>/callback/` and `/login/` to `amaze-portal` (step 3.9). |
| `admin-user`: `no platform account (run migrate, then admin-user -create-platform <name>)` | Add `-create-platform "<name>"` (step 3.10). |
| `seed`: `refusing to seed: APP_ENV must be dev or staging` | Intended in production; for a sign-off pass `-e APP_ENV=staging`. |
| `/readyz` 503 | The JSON body names the dependency (postgres, redis, kafka, clickhouse). |
| A service exits on start with a topic error | Topics are not auto-created: run `topics` (step 3.8). |
| Nothing moves for a new vendor | Wait 30 s. The vendor must not be disabled and needs an enabled bind or an HTTP profile. Look in the connector log for `route topic not created; vendor not started` or `vendor stopped; retrying at the next rescan`. |
| HAProxy does not start | No PEM in `deploy/ha/lb/certs/https/`, or `certs/smpp.pem` missing. See `docker compose -f lb.yml --env-file .env logs haproxy`. |
| HTTP vendor's receipts get 404 | The URL must be `https://<DLR_HOST>/dlr/<vendor>?token=…`; the vendor must be running on the connector (not disabled, HTTP profile saved). |
| `etcdctl auth enable` refused | Grant the root role first: `etcdctl user grant-role root root` (step 3.5). |
| `pg_wal` keeps growing | The pgBackRest stanza does not exist yet: run `backup.sh init` on the leader. Or archiving fails: `backup.sh check`. |
| Redis refuses writes (`NOREPLICAS`) | `min-replicas-to-write 1`: the primary sees no replica in sync (both down or more than 10 s behind). Bring a replica back. |
| ClickHouse read-only, reports stale | Keeper quorum lost (two replicas down). History waits in Kafka (72 h) and is written when quorum returns. |
| SMPP customers get `ESME_RBINDFAIL` | Every bind refusal returns this status, by design. Check credentials and IP allow lists; 10 failures a minute from one address block it for 5 minutes; 64 connections per address; bind within 10 s. |
| SMPP connections through the balancer are refused | `SMPP_PROXY_PROTOCOL_FROM` does not list the balancer's address. Direct connections from a listed address without a PROXY header are refused as well. |
| Messages refused `dlt_template`, `dlt_tm_chain` | DLT data missing for the account (templates, TM chain); see [phase3-dlt.md](phase3-dlt.md). |
| Load or soak stops with `insufficient_balance` | Seed or top up more balance. |
| Balances in Redis and PostgreSQL disagree | Run `wallet-resync -dry-run`, then without it. |

---

## Gaps in the repository

Things a production install needs that the repository does not provide yet. Each is noted where it comes up above.

1. **The HA Docker stack has not been run end to end.** See the warning at the top.
2. **No production Keycloak deployment.** Only the dev server in `deploy/docker-compose.yml`.
3. **No certificate automation.** Certificates in `deploy/ha/lb/certs/https/` are copied and renewed by hand (no ACME client), on both LB VMs.
4. **No backup alerts.** The alert rules do not watch PostgreSQL archiving, backup age or ClickHouse `system.backups` (step 3.13).
5. **CI builds images but pushes none.** Push them yourself, or build on each VM.
6. **No production restore-check script.** `scripts/ha-restore-check.sh` works on the local rehearsal tier only.
7. **No log rotation or shipping.**
