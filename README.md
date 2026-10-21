# Amaze A2P SMS

A carrier-grade A2P SMS platform: customer SMPP (3.3/3.4/5.0) and HTTP in, vendor SMPP and HTTP out, built for 5,000 TPS sustained and 10,000 burst. Launching in India with full DLT.

Status: **Phase 1, Core messaging (slices 1 and 2).** Messages go end to end: customer SMPP or HTTP → Kafka → router → vendor SMPP or HTTP connector → delivery receipt back to the customer (deliver_sm or signed webhook), with wallet holds, ledger charges and history in ClickHouse. Customers can be onboarded through the admin API. Tested at 4,859 accepted TPS on a single 4-core box with zero loss. See [docs/phase1-core-messaging.md](docs/phase1-core-messaging.md) for what is built, the test results and what comes next.

## Layout

| Path | What |
| --- | --- |
| `cmd/smpp-server`, `cmd/http-api` | Customer edges: auth, limits, balance hold, write to Kafka |
| `cmd/admin-api` | Accounts, top-ups, credit limits, rate cards, SMPP credentials, API keys, webhooks (dev only until auth) |
| `cmd/router` | Picks a vendor (LCR / priority / weighted) |
| `cmd/connector` | Sends to one vendor (`VENDOR_ID`) over SMPP or HTTP; journals its DLRs |
| `cmd/webhook-dispatcher` | Signed delivery-status webhooks to HTTP customers, with retries |
| `cmd/ch-writer` | Message history to ClickHouse |
| `cmd/dlr-processor` | Matches DLRs, message state, customer receipts, expiry of messages with no DLR |
| `cmd/rater` | Settles holds, writes the ledger in batches |
| `cmd/migrate` | Applies `migrations/postgres` |
| `cmd/devkafka`, `cmd/smpp-sim`, `cmd/http-sim`, `cmd/smpp-load`, `cmd/devseed` | Dev tools: in-memory Kafka, SMPP and HTTP vendor simulators, load generator, demo data |
| `cmd/billing-check`, `scripts/signoff.sh`, `deploy/vm/` | 5,000 TPS sign-off kit for plain VMs ([docs/signoff-5k.md](docs/signoff-5k.md)) |
| `cmd/admin-user` | Grants the first platform owner (then use the admin API) |
| `internal/smpp/{pdu,session,gsm7}` | Own SMPP 3.3/3.4/5.0 library and GSM-7/UCS-2 encoding |
| `internal/ingest`, `internal/msgstore`, `internal/routing`, `internal/connector`, `internal/dlr` | Message path packages |
| `internal/billing/wallet` | Redis Lua holds: available = balance + credit limit − holds |
| `internal/billing/credit` | Credit limit changes (platform: any account; reseller: own sub-accounts within its balance) |
| `internal/platform/*` | Config, logging, health, metrics, Kafka, Redis, PostgreSQL |
| `migrations/` | PostgreSQL (tenancy, wallet, DLT, routing) and ClickHouse schemas |
| `api/openapi.yaml` | Customer HTTP API contract |
| `deploy/` | Docker Compose dev stack, Kafka topic script |
| `docs/` | Architecture, wallet and credit, India DLT, ADRs |

## Run locally

With Docker (Go not needed on the host):

```sh
make up                       # postgres, redis, redpanda, clickhouse, simulator, seed, all services
docker compose -f deploy/docker-compose.yml logs seed   # prints the demo HTTP API key
make down
```

Without Docker (Linux, WSL or macOS; needs Go 1.26+, PostgreSQL with database `sms` for user sms/sms, Redis on 6379). This wipes the `sms` database and Redis:

```sh
scripts/dev-local.sh up       # builds, starts in-memory Kafka, simulator, all services, seeds demo data
TPS=2000 scripts/dev-local.sh load
scripts/dev-local.sh down
```

Then send a message:

```sh
# SMPP: bind to localhost:2775 as system_id=demo password=demo123
# HTTP:
curl -X POST localhost:8080/v1/sms -H "X-API-Key: <key from seed output>" \
  -d '{"to":"+919812345678","sender":"AMZTST","text":"Your OTP is 482913","pe_id":"1101234567890123456","template_id":"1107160000000012345"}'
curl localhost:8080/v1/sms/<id> -H "X-API-Key: <key>"
```

Ports: SMPP 2775, HTTP 8080, admin 8081, HTTP-vendor DLR callback 8090, SMPP simulator 2776, HTTP simulator 2777, ClickHouse 8123, ops (`/healthz`, `/readyz`, `/metrics`) 9101–9110.

Without Docker, ClickHouse is optional: `pip install chdb` into a virtualenv and run `CHDB_PYTHON=<that python> scripts/dev-local.sh up` to add a ClickHouse stand-in and the ch-writer. `ROUTE=httpsim` sends the demo customer's traffic through the HTTP vendor simulator.

Onboard a customer through the admin API. Sign-in is Keycloak (OIDC bearer tokens, see [docs/admin-auth.md](docs/admin-auth.md)); in local dev without Keycloak, send `X-Actor-Account` = platform account ID and `X-Actor-User` = your email instead:

```sh
POST /v1/accounts                       {"name":"Acme","role":"customer","rate_card_id":"<GET /v1/rate-cards>"}
POST /v1/accounts/{id}/topups           {"amount":"5000","reference":"NEFT 123"}   + Idempotency-Key header
POST /v1/accounts/{id}/api-keys         {"name":"prod"}                            -> key shown once
POST /v1/accounts/{id}/smpp-credentials {"system_id":"acme","password":"acme1234","tps":100,"max_binds":4}
PUT  /v1/accounts/{id}/webhook          {"url":"https://acme.example/sms-status"}  -> signing secret shown once
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

## Next

Run the 5,000 TPS sign-off on VMs ([docs/signoff-5k.md](docs/signoff-5k.md)); phase 2 (billing and commercial, [docs/phase2-billing.md](docs/phase2-billing.md)); then phase 3 (DLT template matching, firewall, HLR/MNP, vendor failover).
