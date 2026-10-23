# Amaze A2P SMS

A carrier-grade A2P SMS platform: customer SMPP (3.3/3.4/5.0) and HTTP in, vendor SMPP and HTTP out, built for 5,000 TPS sustained and 10,000 burst. Launching in India with full DLT.

Status: **Phase 1, Core messaging (slices 1 and 2).** Messages go end to end: customer SMPP or HTTP → Kafka → router → vendor SMPP or HTTP connector → delivery receipt back to the customer (deliver_sm or signed webhook), with wallet holds, ledger charges and history in ClickHouse. Customers can be onboarded through the admin API. Tested at 4,859 accepted TPS on a single 4-core box with zero loss. See [docs/phase1-core-messaging.md](docs/phase1-core-messaging.md) for what is built, the test results and what comes next.

Documentation: [admin user guide](docs/admin-guide.md), [setup and installation guide](docs/setup-guide.md), [API reference](docs/api-reference.md).

## Layout

| Path | What |
| --- | --- |
| `cmd/smpp-server`, `cmd/http-api` | Customer edges: auth, limits, balance hold, write to Kafka |
| `cmd/admin-api` | Accounts, top-ups, credit limits, rate cards, SMPP credentials, API keys, webhooks (dev only until auth) |
| `cmd/router` | Picks a vendor (LCR / priority / weighted) |
| `cmd/connector` | Sends to vendors over SMPP or HTTP and journals their DLRs. `VENDOR_ID=*` (default) runs every vendor that is not disabled and has an enabled bind or HTTP profile, picks up portal changes within `VENDOR_RESCAN` (30 s) and creates each vendor's route topic; `VENDOR_ID=<id>` runs one vendor |
| `cmd/webhook-dispatcher` | Signed delivery-status webhooks to HTTP customers, with retries |
| `cmd/ch-writer` | Message history to ClickHouse |
| `cmd/dlr-processor` | Matches DLRs, message state, customer receipts, expiry of messages with no DLR |
| `cmd/rater` | Settles holds, writes the ledger in batches |
| `cmd/migrate` | Applies `migrations/postgres` |
| `cmd/devkafka`, `cmd/smpp-sim`, `cmd/http-sim`, `cmd/smpp-load`, `cmd/devseed` | Dev tools: in-memory Kafka, SMPP and HTTP vendor simulators, load generator, demo data |
| `cmd/billing-check`, `scripts/signoff.sh`, `deploy/vm/` | 5,000 TPS sign-off kit for plain VMs ([docs/signoff-5k.md](docs/signoff-5k.md)) |
| `cmd/admin-user` | Creates the platform account on a new installation (`-create-platform`) and grants the first platform owner (then use the admin API) |
| `internal/smpp/{pdu,session,gsm7}` | Own SMPP 3.3/3.4/5.0 library and GSM-7/UCS-2 encoding |
| `internal/ingest`, `internal/msgstore`, `internal/routing`, `internal/connector`, `internal/dlr` | Message path packages |
| `internal/billing/wallet` | Redis Lua holds: available = balance + credit limit − holds |
| `internal/billing/credit` | Credit limit changes (platform: any account; reseller: own sub-accounts within its balance) |
| `internal/billing/hierarchy`, `internal/billing/ledger` | Reseller chain for two-level charging; top-ups, reseller-to-sub-account transfers, dispute credits ([docs/phase2-billing.md](docs/phase2-billing.md)) |
| `portal/` | Next.js + TypeScript portals (admin, reseller, customer), served by admin-api ([docs/phase4-portals.md](docs/phase4-portals.md); user guide [docs/admin-guide.md](docs/admin-guide.md)) |
| `cmd/alerter`, `internal/alerts` | Low-balance alerts by e-mail (SMTP), SMS and webhook |
| `internal/vendorhealth`, `internal/network`, `internal/firewall` | Vendor failover, MNP / number series / HLR, firewall rules and SMS-pumping limits ([docs/phase3-network.md](docs/phase3-network.md)) |
| `internal/dlt` | India DLT: template matching, entity/header/category checks, whitelists, TM chain, CSV import, vendor error codes ([docs/phase3-dlt.md](docs/phase3-dlt.md)) |
| `internal/billing/deals`, `internal/billing/recon` | Customer deals (capped volume reserved in Redis), vendor deal progress, vendor invoice reconciliation ([docs/phase2-deals-disputes.md](docs/phase2-deals-disputes.md)) |
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
  -d '{"to":"+919812345678","sender":"AMZTST","text":"Your OTP is 482913. Do not share it with anyone. -Amaze","pe_id":"1101234567890123456","template_id":"1107160000000012345"}'
curl localhost:8080/v1/sms/<id> -H "X-API-Key: <key>"
```

Portal: build it once (`cd portal && npm ci && npm run build`), then open http://localhost:8081 (dev sign-in lists the seeded users).

Ports: SMPP 2775, HTTP 8080, admin API and portal 8081, HTTP-vendor DLR callback 8090, SMPP simulator 2776, HTTP simulator 2777, ClickHouse 8123, ops (`/healthz`, `/readyz`, `/metrics`) 9101–9107 and 9109–9111 (one connector on 9107 runs every vendor).

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

Run the 5,000 TPS sign-off on VMs ([docs/signoff-5k.md](docs/signoff-5k.md)). Phase 2 is done: slice 1 (reseller billing, rate cards, transfers, statements and usage, [docs/phase2-billing.md](docs/phase2-billing.md)) and slice 2 (customer and vendor deals, customer and vendor disputes, [docs/phase2-deals-disputes.md](docs/phase2-deals-disputes.md)). Phase 3 is done: India DLT enforcement ([docs/phase3-dlt.md](docs/phase3-dlt.md)) and vendor failover, HLR/MNP and the firewall ([docs/phase3-network.md](docs/phase3-network.md)). Still open from the blueprint's routing and compliance phase: MO delivery to customers and hubbing, and country profiles beyond India. Phase 4 is done ([docs/phase4-portals.md](docs/phase4-portals.md)): slice 1, the admin, reseller and customer portals with reports, live monitoring, low-balance alerts and reseller white-label; slice 2, screens for DLT registrations, deals, disputes and rate cards, and the platform's operations pages (vendor onboarding, routes, firewall, vendor billing, number data, error codes). Phase 5 slice 1 is done ([docs/phase5-ha.md](docs/phase5-ha.md)): the replicated data tier on VMs (PostgreSQL with Patroni, Redis with Sentinel, ClickHouse replicas with Keeper, HAProxy), backups to S3 and NFS with tested restores, and failover drills. Slice 2 is done ([docs/phase5-security.md](docs/phase5-security.md)): a security audit with its fixes, and the tooling for the 10,000 TPS 24-hour soak ([docs/soak-test.md](docs/soak-test.md)), rehearsed on one machine; the 24-hour run itself needs the production-sized VMs.
