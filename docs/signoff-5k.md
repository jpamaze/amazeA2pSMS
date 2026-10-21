# 5,000 TPS sign-off runbook

The sign-off proves the platform sustains 5,000 TPS for 30 minutes and a 10,000 TPS burst for 5 minutes on production-shaped hardware, with no message loss and exact billing. It runs on plain VMs with Docker Compose; every file is in `deploy/vm/`.

Single-box results are in `docs/phase1-core-messaging.md` (4,515–4,859 TPS on one 4-vCPU container running everything). Those are a lower bound, not the sign-off.

## Layout

| VM | Role | Size | Compose file |
| --- | --- | --- | --- |
| kafka-1, kafka-2, kafka-3 | Redpanda brokers (Kafka API), replication 3 | 8 vCPU, 32 GB, 500 GB NVMe | `kafka.yml` |
| data-1 | PostgreSQL 16 + Redis 7 | 8 vCPU, 32 GB, 500 GB NVMe | `data.yml` |
| ch-1 | ClickHouse | 8 vCPU, 32 GB, 1 TB NVMe | `clickhouse.yml` |
| app-1, app-2 | All platform services (each VM runs every service) | 8 vCPU, 16 GB | `app.yml` |
| sim-1 | SMPP and HTTP vendor simulators, load generator | 8 vCPU, 16 GB | `sim.yml` |
| mon-1 | Prometheus + Grafana | 4 vCPU, 8 GB, 200 GB | `monitoring.yml` |

All 10 VMs sit in one private network (one availability zone is fine for the sign-off) with these ports open between them: 9092 and 33145 (Kafka), 9644 (Redpanda metrics), 5432, 6379, 8123, 2775–2777, 8080, 8081, 8090, 9100–9110. Run `node-exporter.yml` on every VM.

Data stores run as single instances here, because the sign-off measures throughput. The production layout replicates every store (PostgreSQL with Patroni, Redis with Sentinel, three ClickHouse replicas, three app VMs behind a load balancer): see [phase5-ha.md](phase5-ha.md) and `deploy/ha/`. The services take the same settings either way, so the sign-off can also run on the HA layout.

## Setup (about 1 hour)

1. On every VM: install Docker Engine 26+ and the Compose plugin, then clone the repository. Raise kernel limits:
   `sysctl -w net.core.somaxconn=65535 fs.file-max=2097152 net.ipv4.ip_local_port_range="10000 65000"`.
2. Copy `deploy/vm/env.example` to `deploy/vm/.env` on every VM. Fill in the IPs and set new secrets (`openssl rand -hex 32` for `SECRETS_KEY`). The file must be identical everywhere.
3. Build the images on one VM and push them to your registry, or build on each VM:
   `docker compose -f deploy/vm/app.yml --env-file deploy/vm/.env build` (and the same for `sim.yml`).
4. Start the infrastructure:
   - kafka-N: `BROKER=N SELF=<this VM IP> docker compose -f deploy/vm/kafka.yml --env-file deploy/vm/.env up -d`
   - data-1: `docker compose -f deploy/vm/data.yml --env-file deploy/vm/.env up -d`
   - ch-1: `docker compose -f deploy/vm/clickhouse.yml --env-file deploy/vm/.env up -d`
   - every VM: `docker compose -f deploy/vm/node-exporter.yml up -d`
5. Initialise, from app-1:
   ```sh
   cd deploy/vm
   docker compose -f app.yml --env-file .env --profile init run --rm migrate
   docker compose -f app.yml --env-file .env --profile init run --rm topics
   docker compose -f app.yml --env-file .env --profile init run --rm seed
   ```
   The seed creates the `demo` SMPP account (password `demo123`), a ₹1 crore balance, a 20,000 TPS bind limit, and routes to the SMPP simulator over 8 binds.
6. Start the simulators on sim-1: `docker compose -f deploy/vm/sim.yml --env-file deploy/vm/.env up -d smpp-sim http-sim`.
7. Start the platform on app-1 and app-2: `NODE=app-1 docker compose -f deploy/vm/app.yml --env-file deploy/vm/.env up -d` (use `NODE=app-2` on the second). Check every service with `curl -s <app>:91xx/readyz` (ports 9101–9110).
8. Monitoring on mon-1: `scripts/signoff.sh render-monitoring`, then `docker compose -f deploy/vm/monitoring.yml --env-file deploy/vm/.env up -d`. Open Grafana at `http://mon-1:3000`, dashboard **SMS sign-off**.

## Run (about 1 hour 15 minutes)

On sim-1:

```sh
scripts/signoff.sh run
```

It runs three stages, each with its own pass/fail gate. Reports go to `deploy/vm/reports/<stage>.json`, and the script stops at the first failure.

| Stage | Load | Pass when |
| --- | --- | --- |
| smoke | 500 TPS, 2 min | 0 refused, ≥ 99% of target accepted, every receipt back within 30 s |
| steady | 5,000 TPS, 30 min | 0 refused, ≥ 99% of target accepted, submit_sm_resp p99 ≤ 100 ms (`MAX_P99`), every receipt back, last receipt ≤ 60 s after sending stops (`MAX_LAG`) |
| burst | 10,000 TPS, 5 min | 0 refused, ≥ 98% accepted, p99 ≤ 250 ms (`BURST_MAX_P99`), every receipt back within 5 min |

Then, on app-1, run the money check with the start time the script printed:

```sh
scripts/signoff.sh billing <since>
```

It passes when no holds are open, Redis and PostgreSQL balances agree for every wallet, ledger charges equal the ClickHouse total of charged messages for every account, and no message is left without a final status.

## Failure drills (during a second steady run)

Start `STEADY_DURATION=20m scripts/signoff.sh run`, then do one drill at each mark. Afterwards, run `billing` again. Expect a few duplicate submits to the vendor after the kill drills (the load report counts receipts, not duplicates), but zero lost messages.

| At | Drill | Command | Expected |
| --- | --- | --- | --- |
| 3 min | Kill a connector | on app-1: `docker kill amaze-app-connector-sim-1` | app-2's connector takes the partitions within about 10 s; restart policy brings app-1's back |
| 6 min | Kill a Kafka broker | on kafka-2: `docker kill amaze-kafka-redpanda-1`, start it again after 2 min | no refused submits (acks=all with 2 of 3 in sync); brief latency spike |
| 10 min | Kill an SMPP edge | on app-2: `docker kill amaze-app-smpp-server-1` | load generator binds on app-2 drop; binds on app-1 keep sending; receipts for app-2's binds wait in Redis until a bind returns |
| 14 min | Vendor outage | on sim-1: `docker stop amaze-sim-smpp-sim-1`, start after 60 s | failover takes `sim` out of routing within about 3 s (`sms_vendor_routable{vendor="sim"} 0`); new and queued traffic moves to `httpsim` (`sms_rerouted_total`); `sim` returns within seconds of restarting; nothing refused at the edge. Receipts `sim` owed when it died are lost with the simulator (a real vendor keeps them) |
| 18 min | Restart PostgreSQL | on data-1: `docker restart amaze-data-postgres-1` | message path continues (no per-message database queries); rater catches up |

The SMPP-edge drill makes the load generator report failed submits on the dropped binds. That is expected, and that run is judged on billing and lost messages only.

## Record the result

| Item | Value |
| --- | --- |
| Date, git commit | |
| VM types and region | |
| steady: accepted TPS, p50/p99, receipt lag | from `reports/steady.json` |
| burst: accepted TPS, p50/p99, receipt lag | from `reports/burst.json` |
| Peak CPU per VM role | Grafana "Host CPU busy" |
| billing check | PASS / FAIL |
| Drills | per drill: pass/fail and notes |

If a stage fails, the dashboard shows where the backlog builds. "Consumer throughput by group" shows which service falls behind. "Service CPU" and "Host CPU busy" show whether it is short of CPU (add an app VM, or move that service to its own VM) or waiting on a dependency (Redis, Kafka).
