# Phase 5 slice 2: security audit and soak test

The blueprint's last hardening items: a security audit, and the 10,000 TPS 24-hour soak test (procedure in [soak-test.md](soak-test.md)).

## How the audit was done

- **Tools:**
  - `gosec` and `staticcheck` over the Go code.
  - `npm audit` over the portal: no known vulnerabilities in production dependencies.
  - `govulncheck` could not reach its vulnerability database from the build machine (the network policy blocks vuln.go.dev). It now runs in CI with staticcheck and `npm audit`, on every push and weekly (`.github/workflows/ci.yml`, job `security`).
- **Manual review of every way in:**
  - **Admin API and portal:** authorization of every route, sign-in, headers.
  - **Customer and vendor edges:** SMPP server and PDU parsing, HTTP API, webhooks, the vendor receipt endpoint, alerts.
  - **Data and deployment:** every PostgreSQL and ClickHouse query that takes input, secrets and cryptography, Docker and compose files, the Keycloak realm.

  Every finding below was confirmed in the code before it was fixed. Each fix has a test.

What was already sound:
- **Tenant isolation and authorization:**
  - Every admin route checks permission and scope.
  - No route reads or changes another tenant's objects (deals, disputes, rate cards, keys, users, routes all scoped).
  - Money moves need the finance role.
- **Queries:** no SQL or ClickHouse injection. Every value is a parameter, or a parsed ID, date or number. The few free-text filters are escaped.
- **Credentials:**
  - SMPP passwords use bcrypt, with a decoy hash for unknown system IDs.
  - API keys are 192-bit random secrets stored as SHA-256 and compared in constant time.
  - Vendor passwords and secrets are sealed with AES-256-GCM.
  - Nothing secret is logged or returned.
- **PDU framing:** bounded (16 bytes to 64 KiB), every length checked.
- **HTTP API:** body capped at 64 KiB, timeouts set.
- **Receipt webhooks:** blocked from private addresses when dialling, no redirects, signed.
- **Images:** distroless, non-root.

## Findings and fixes

| # | Severity | Finding | Fix |
| --- | --- | --- | --- |
| 1 | High | **Sign-in failed open.** `APP_ENV` defaulted to `dev` when unset. A production admin-api without `OIDC_ISSUER` then trusted `X-Actor-*` headers (anyone could act as the platform), and secrets were sealed with the public dev key. | `APP_ENV` defaults to `production`; every dev behaviour needs `APP_ENV=dev` explicitly. devseed never uses the dev key outside dev. The HA env file sets `APP_ENV=production`. |
| 2 | High | **Script injection on the login page.** `?next=` went straight into `location.replace`: `?next=javascript:...` ran in the portal origin and could read the session's access token. | Post-sign-in destinations are restricted to same-origin paths (`safePath`) on the login page, before sign-in and after the callback. A Content-Security-Policy now also limits where scripts can send data (#9). |
| 3 | High | **SMPP UDH bypass.** A message carrying a UDH (sender-split) skipped DLT content matching, was billed as one segment whatever its length, and could carry about 60 KB of text (message_payload) or port-addressed/WAP content to the vendor. | A UDH may hold exactly one concatenation element with sane numbers, otherwise `ESME_RINVESMCLASS`. A part must fit one SMS (160 GSM septets / 140 octets including the UDH). Each part's text must fit the template it names: a new fragment matcher accepts only text that could be a piece of a message matching the template. TLV values are capped at 64 KiB when encoded. |
| 4 | High | **SMPP memory exhaustion before bind.** A peer streaming enquire_link (or junk) without reading the replies made the session start a goroutine per request. | Replies and handlers share one in-flight limit, so a peer that does not read stalls its own session, which the idle check then closes. A panic in a handler closes that session only. |
| 5 | High | **Patroni REST API and etcd had no authentication.** Anyone reaching a PostgreSQL VM could rewrite `pg_hba` through Patroni's configuration or etcd, or force a switchover. | Patroni's REST API requires a password for every changing call (health checks stay open). etcd runs with authentication (Patroni signs in). Firewall guidance in the compose file. |
| 6 | High | **Redis Sentinel had no password, and neither did the sign-off Redis.** `SENTINEL MONITOR` could point every service at an attacker's Redis (balances, and the Redis password sent to it). | Sentinel has `requirepass` (`REDIS_SENTINEL_PASSWORD`, followed by the platform). The sign-off Redis requires `REDIS_PASSWORD`. |
| 7 | Medium | **SSRF from low-balance alert webhooks.** These used a plain HTTP client: private addresses, redirects, and error messages that revealed internal ports. | The alerter uses the receipt-webhook client: public addresses only (checked at dial time), no redirects. The blocked ranges now also cover CGNAT (cloud metadata), IETF and benchmark ranges, NAT64 and 6to4. |
| 8 | Medium | **A malformed vendor receipt crashed the connector.** Invalid UTF-8 shifted the parser's offsets and caused a panic. An unexpected PDU answering a submit_sm also caused a panic. | ASCII-only lowercasing keeps offsets; the response type is checked. Handler panics are recovered per session (#4). |
| 9 | Medium | **Headers.** API responses were cacheable, and there was no CSP or HSTS. | `Cache-Control: no-store` on every API response. A CSP on portal pages: scripts, styles and API calls stay on the portal and the sign-in server; no framing. HSTS on HTTPS. admin-api read, write and idle timeouts. |
| 10 | Medium | **SMPP binds.** No limit on failed binds per address. A different status for a right password from a disallowed address (a password oracle). Binds pipelined on one connection each took a bind slot and left stale receivers. No limit on connections per address, and no deadline to bind. | One status (`ESME_RBINDFAIL`) for every refusal. 10 failures a minute from an address refuse it for 5 minutes. One bind at a time per connection. 64 connections per address. A connection not bound within 10 s is closed (`SMPP_MAX_BIND_FAILS`, `SMPP_MAX_CONNS_PER_IP`, `SMPP_BIND_TIMEOUT`). |
| 11 | Medium | **Client addresses behind the load balancer.** In TCP mode every customer would look like the balancer (per-address limits, IP allow lists). | PROXY protocol v2 from HAProxy (`send-proxy-v2`). The SMPP server requires it from the listed balancers and refuses it from anyone else (`SMPP_PROXY_PROTOCOL_FROM`). SMPP over TLS: port 3550 on the balancer, or natively (`SMPP_TLS_CERT`/`SMPP_TLS_KEY`). |
| 12 | Medium | **A reseller could lift a platform suspension** of its sub-account. | Who suspended an account is recorded (`accounts.suspended_by`). Only they, or the platform, can reactivate it. |
| 13 | Medium | **A reseller could claim any portal domain**, including the platform's, and brand the main sign-in page. | Only the platform sets portal domains (it also adds the certificate and sign-in redirect). A reseller editing its brand keeps its domain. |
| 14 | Medium | **E-mail header injection.** A reseller's brand name went into alert e-mail subjects unchecked. | Control characters are refused in brand and account names, and stripped from mail headers (subjects encoded). |
| 15 | Medium | **CSV formula injection in statement downloads.** References typed by another account could run as spreadsheet formulas. | Free-text cells starting with `= + - @` are prefixed with `'`. |
| 16 | Low | The vendor receipt token was shown to read-only platform users. | Shown to admins only. |
| 17 | Low | **Unauthenticated ops surfaces.** pprof was on by default in `app.yml`. HAProxy stats had no password. PostgreSQL ports listened on the public side of the balancer. | pprof off by default. Stats behind a password on the private address. PostgreSQL listeners on the private address only. |
| 18 | Low | **Weak spots in the vendor receipt endpoint.** A blank configured token would have accepted anyone, and it had no read/write timeouts. | Blank token refused. The token can come in an `X-DLR-Token` header (kept out of access logs). Timeouts and a 64 KiB body limit added. |
| 19 | Low | **OIDC checks.** Tokens without `email_verified` were accepted, and an empty `OIDC_AUDIENCE` disabled the audience check. | `email_verified` must be true; admin-api refuses to start without an audience. |
| 20 | Low | **Error detail leaks.** HTTP API 503s returned internal error text (hosts, ports). | Generic message; details in the log. |
| 21 | Low | **Shared idempotency keys.** Transfer and top-up keys were global, so another account's key could block a transfer. | Keys are scoped to the paying account. |

## Still open (accepted or for later)

- **Plaintext traffic inside the private network:**
  - Inside the platform's private network, PostgreSQL (`sslmode=disable`), Redis, Kafka and ClickHouse traffic is not encrypted.
  - Customers' SMPP and HTTPS are encrypted at the balancer (or natively for SMPP).
  - Enable TLS on the data stores when the network is shared with anything else.
- **PE ID ownership:** DLT PE IDs are registered first come, first served. A PE ID cannot be taken from its owner, but someone could register a competitor's first. Verifying PE ownership needs the operators' DLT portals (blueprint: "sync with operator DLT portals where they offer an API").
- **E-mail enumeration:** user e-mails are unique across tenants, so adding a user who exists elsewhere returns "already exists". Invitations by e-mail confirmation would close this.
- **Audit trail gaps:** the admin audit log takes `X-Forwarded-For` as given, and does not record refused or read-only requests.
- **Sealed secrets:** they are not bound to their row (no associated data). Someone able to write the database could swap two vendors' sealed passwords.
- **Concurrent duplicate submits:** a duplicate HTTP `Idempotency-Key` arriving while the first request is still being accepted gets the first request's ID even if that request then fails.
- **API key revocation:** takes effect within 60 s (cache).
- **Keycloak realm:** `deploy/keycloak/amaze-realm.json` is a development starting point. `docs/admin-auth.md` lists what to change for production: remove the demo user, turn off direct grants, set real redirect URIs, require OTP.

## Verification

**Tests:**
- New and changed tests:
  - **SMPP server:** one bind status for every refusal, the bind throttle, the per-address connection limit, the bind deadline, PROXY protocol from balancers only, and TLS.
  - **SMPP session:** a peer that never reads stays bounded, and a handler panic closes only its session.
  - **PDU:** receipts with invalid UTF-8; TLV length cap.
  - **DLT and ingest:** fragment matching, and UDH rules (one concatenation element, sane numbers, a part fits one SMS).
  - **Webhooks:** the blocked address ranges.
  - **Admin API:** domain rules, suspension ownership, security headers, CSV cells, OIDC `email_verified` and audience.
  - **Ledger:** scoped idempotency keys.
  - **Soak tooling:** the load generator's histogram and soak-report's checks.
- Full suite: `go test -race -count=1 ./...` three times against Redis and PostgreSQL: 27 packages, every run passed, nothing skipped. `go vet`, `gofmt` and `staticcheck` clean.
- **gosec:** what remains was checked and left as is:
  - integer conversions are bounded beforehand (G115);
  - file paths come from flags or are cleaned and tested (G703);
  - report files are written 0644 on purpose (G306);
  - one test URL looks like a credential (G101);
  - unchecked `Close` errors on read paths (G104).

**Portal:** headless Playwright walkthroughs of the portal. With the CSP on, no console errors and no blocked requests. `/login?next=javascript:...` lands on `/`. The platform sets a reseller's domain; the reseller sees it read-only. Large amounts no longer overflow the dashboard on a phone.

**Soak rehearsal** (dev stack, in-memory Kafka, one 4-vCPU machine; `smpp-load -interval 5m -reconnect`, then `soak-report` and `billing-check`):

| Item | Result |
| --- | --- |
| Load | 500 TPS for 40 min: 1,198,202 accepted, 0 refused, 0 reconnects |
| Latency | p50 6.9 ms, p99 55.3 ms overall; worst 5-minute p99 102.4 ms (second checkpoint), then 37–47 ms |
| Receipts | all 1,198,202; the last 0.2 s after sending stopped; 118–174 outstanding at every checkpoint |
| Memory, goroutines (warm-up hour against the last) | flat: largest RSS growth 29 → 31 MiB (+5%), goroutines 135 → 152 (+13%) |
| Messages in flight | 859 → 850 |
| Restarts, history rows dropped, batches retried | 0, 0, 0 |
| billing-check | PASS: 1,198,202 charged messages, Redis = PostgreSQL, ledger = ClickHouse |

The platform services stayed flat. Only the in-memory Kafka grew (to 4.3 GB), as it keeps every record, which is why a local soak stays short. The first rehearsal ran the demo account out of money after about 833,000 messages: soak-report flagged it as refusals, and the seed balance is now configurable (`SEED_BALANCE`).

**HA rehearsal with the hardened configuration** (`scripts/ha-local.sh`, `scripts/ha-drill.sh all`, `scripts/ha-restore-check.sh`):
- Authentication is on and refuses outsiders:
  - etcd refuses unauthenticated reads ("user name is empty").
  - Patroni's REST API answers 401 to an unauthenticated `PATCH /config`; health checks still answer 200.
  - Sentinel answers `NOAUTH`.
- Patroni elects, rewinds and rejoins through the authenticated etcd, and `patronictl` works with its password.
- The rehearsal found that etcd 3.4 refuses `auth enable` until the root user has the root role. The script and the instructions in `deploy/ha/postgres.yml` now grant it first.
- Drills, all with billing-check PASS:

| Drill | Failover | Submits refused | Afterwards |
| --- | --- | --- | --- |
| PostgreSQL leader crash | new leader 18.0 s; writes 18.8 s; HAProxy 19.5 s | 0 of 42,631 | old leader back via pg_rewind |
| Redis primary crash | Sentinel promoted in 4.7 s | 2,185 (the promotion window, as in slice 1) | old primary back as a replica; `wallet-resync` found nothing to repair |
| ClickHouse replica crash | writer moved on | 0 | 170,196 rows on all three |
| Keeper quorum lost for 30 s | writable again after 36.2 s | 0 | 212,512 rows on all three |

- Restores all PASS:
  - PostgreSQL point-in-time restore from S3 and from NFS: 6,435 ledger entries, same total.
  - ClickHouse from S3 and from disk: 212,512 rows.
  - Redis snapshot: 680,925 keys.
