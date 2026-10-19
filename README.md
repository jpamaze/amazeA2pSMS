# Amaze A2P SMS

A carrier-grade A2P SMS platform: customer SMPP (3.3/3.4/5.0) and HTTP in, vendor SMPP and HTTP out, built for 5,000 TPS sustained and 10,000 burst. Launching in India with full DLT.

Status: **Phase 0, Foundations.** The services start, report health, and consume their Kafka topics. The SMPP protocol, routing, DLT matching and connectors are stubs marked `TODO(phase N)`. Built and tested so far: wallet holds, credit limits with the reseller rules, and the database schema.

## Layout

| Path | What |
| --- | --- |
| `cmd/smpp-server`, `cmd/http-api` | Customer edges: auth, limits, balance hold, write to Kafka |
| `cmd/admin-api` | Accounts, config, credit limits (`PUT /v1/accounts/{id}/credit-limit`, dev only until auth) |
| `cmd/router`, `cmd/rater`, `cmd/dlr-processor`, `cmd/connector` | Kafka consumers on the message path |
| `cmd/migrate` | Applies `migrations/postgres` |
| `internal/billing/wallet` | Redis Lua holds: available = balance + credit limit − holds |
| `internal/billing/credit` | Credit limit changes (platform: any account; reseller: own sub-accounts within its balance) |
| `internal/platform/*` | Config, logging, health, metrics, Kafka, Redis, PostgreSQL |
| `migrations/` | PostgreSQL (tenancy, wallet, DLT, routing) and ClickHouse schemas |
| `api/openapi.yaml` | Customer HTTP API contract |
| `deploy/` | Docker Compose dev stack, Kafka topic script |
| `docs/` | Architecture, wallet and credit, India DLT, ADRs |

## Run locally

Requires Go 1.26+ and Docker.

```sh
make up          # postgres, redis, redpanda, clickhouse, topics, migrate, all services
curl localhost:9103/readyz   # admin-api readiness (ops ports 9101-9107)
make down
```

Without Docker, against local Postgres and Redis:

```sh
export POSTGRES_URL=postgres://sms:sms@localhost:5432/sms?sslmode=disable
make migrate
APP_ENV=dev go run ./cmd/admin-api
```

## Test

```sh
make test   # unit + integration; integration tests skip when Redis/Postgres are unreachable
```

Integration tests use `REDIS_TEST_ADDR` (default `localhost:6379`) and `POSTGRES_TEST_URL` (default `postgres://sms:sms@localhost:5432/sms_test`). **The test database is wiped on every run.**

## Applying a bundle from a Claude session

Claude sessions that cannot push hand over a git bundle. On the main dev machine the repo is `D:\ZAPIM\SMPP\amazeA2pSMS` and the bundle is saved as `D:\ZAPIM\SMPP\amazeA2pSMS.bundle`:

```powershell
powershell -ExecutionPolicy Bypass -File D:\ZAPIM\SMPP\amazeA2pSMS\scripts\sync-from-bundle.ps1
```

The script verifies the bundle, fast-forwards `main` and pushes. Pass `-NoPush` to skip the push, or `-RepoPath` / `-BundlePath` to use other paths.

## Next: Phase 1, Core messaging (weeks 5–16)

SMPP PDU codec and customer binds, HTTP send, vendor SMPP and HTTP connectors with per-vendor TLV profiles, basic routing, DLR journal and delivery, ClickHouse writer, SMPP simulator, 5,000 TPS load-test gate.
