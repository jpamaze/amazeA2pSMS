# Phase 3 slice 2: vendor failover, HLR/MNP, firewall

This completes phase 3. DLT enforcement (slice 1) is in [phase3-dlt.md](phase3-dlt.md).

## Vendor failover (reliability rule 8)

A vendor that goes down, slows, or drops below its delivery-rate threshold is taken out of routing within seconds, and its queued traffic moves to the next route.

```
connector ──report every 2 s──▶ Redis vhealth ◀── dlr-processor (receipts per vendor per minute)
                                     │
               router: evaluate every 2 s against each vendor's policy
                                     │ verdict
                    ┌────────────────┴────────────────┐
                    ▼                                 ▼
         routing skips the vendor         connector stops sending: unsent and
                                          failed messages go to sms.reroute
                                          and the router picks another vendor
```

| Signal | Out of routing when | Default |
| --- | --- | --- |
| Binds | No connector reporting, or 0 sending binds up (SMPP) | always on |
| Errors | Temporary failures (timeouts, throttling, lost binds, HTTP 5xx, connection errors) above `max_error_pct` of submits in 30 s, at least 20 submits | 50% |
| Latency | p95 submit time above `max_submit_ms`, at least 20 submits | 3,000 ms |
| Delivery rate | Delivered share of final receipts below `min_dlr_pct` over `dlr_window_min`, at least 200 receipts | off (0) |

- **Recovery:** a vendor taken out for binds returns as soon as a bind is up. One taken out for errors, latency or delivery rate stays out for `cooldown_s` (default 30 s), then gets traffic again; if still bad, it goes straight back out.
- **Rerouting:** a message the connector has not sent goes back to the router with the vendor on its `avoid` list. So does a message whose send failed with a temporary error after its retries. The router picks the next route.
  - At most 2 reroutes per message; after that it is tried where it is.
  - A message refused by the vendor (invalid number, bad content) is not rerouted.
  - A multipart message with some parts already accepted is not rerouted.
- **Nothing healthy left:** routing falls back to vendors out of routing, then to avoided ones, rather than refusing. Queuing at a vendor that may recover beats refusing.
- **Per-vendor policy:** `PUT /v1/vendors/{id}/failover`. `GET /v1/vendors/health` shows each vendor's verdict, policy and connector reports.
- **Metrics:** `sms_vendor_routable{vendor}` and `sms_rerouted_total{from_vendor,reason}`. The router logs every verdict change.

**Measured on the dev stack** (500 TPS, the cheaper SMPP vendor killed mid-run and restarted 20 s later):
- Out of routing 2.6 s after the kill; back in routing 1.9 s after the restart.
- 0 refused submits, p99 18 ms throughout.
- 1,388 queued messages were rerouted to the HTTP vendor, and traffic returned to the SMPP vendor after it came back.
- The 94 receipts the killed simulator still owed were lost with it. The simulator keeps them only in memory; real vendors hold them, and unmatched messages expire through the reconcile job.

## HLR / MNP

The network of each number is resolved at submit, before pricing and routing:

1. **MNP:** a ported number's current network, from local MNP data in Redis.
2. **Number series:** the operator owning the number's prefix (longest match), held in memory by each edge.
3. **Default:** `404` / `*`.

- **Country fallback:** rate cards, routes and deals keyed on `404` / `*` apply to every Indian network (MCC 404 and 405). An entry for the exact network wins, so you can price or route one operator differently without repeating the country-wide entries.
- **HLR per route:** a route with `hlr: true` makes the router look the number up before choosing a vendor.
  - **Cache:** answers are kept in Redis for 48 h (`HLR_CACHE_TTL`).
  - **Speed:** lookups run concurrently per batch (`HLR_CONCURRENCY`, 2 s timeout).
  - **Override:** the HLR network overrides MNP/series for routing.
  - **Refusing numbers:** with `hlr_reject: true`, numbers the HLR reports invalid or absent are refused before reaching a vendor (`HLR_INVALID` / `HLR_ABSENT`, hold released).
  - **Failures:** a failed lookup never blocks a message.
- **Provider:** HLR providers differ, so the client is configured, not coded (router env):
  - `HLR_URL` with `{msisdn}`;
  - `HLR_AUTH_HEADER` / `HLR_AUTH_VALUE`;
  - where the answer is: `HLR_MCCMNC_PATH`, or `HLR_MCC_PATH` and `HLR_MNC_PATH`, plus `HLR_STATUS_PATH`;
  - which status values mean ok, absent or invalid.
  - An empty `HLR_URL` turns HLR off.

| Admin API | What |
| --- | --- |
| `POST /v1/number-series/import[?replace=true]` | CSV `prefix,mcc,mnc[,operator,circle]` (national prefixes, 2–9 digits); edges reload every 5 minutes |
| `POST /v1/mnp/import` | CSV `msisdn,mcc,mnc`, streamed into Redis (an empty mnc removes a number that ported back); effective at once; bad lines are reported and skipped |
| `GET /v1/numbers/{msisdn}` | How a number resolves: series, MNP, the result ingest uses, and any cached HLR answer |
| `GET/POST /v1/routes`, `DELETE /v1/routes/{id}` | Routes, including `hlr` and `hlr_reject` |

- **MNP storage:** MNP data is stored as buckets of 100 numbers (Redis small-hash encoding). Budget roughly 2–3 GB of Redis memory per 100 million ported numbers. Our data source is the MNP provider's dump; import it daily.
- **Known limitation:** an HLR override changes the route and the vendor cost, but the price was set at submit from MNP/series, and the message record keeps that network.

## Firewall

Checked at submit after DLT. Rules are reloaded every 30 s; the velocity check is one Redis script call per message, sent in the same round trip as the MNP lookup.

**Block lists** (`POST /v1/firewall/rules`, platform only; for every account or one account; `reject`, or `flag` to only count in `sms_firewall_hits_total`):

| Kind | Pattern | Reason |
| --- | --- | --- |
| `sender` | glob, case-insensitive, e.g. `*BANK` | `fw_sender` |
| `content` | phrase (whole words, any spacing, case-insensitive), or `re:<regexp>` | `fw_content` |
| `url` | domain; blocks it and its subdomains wherever a link appears | `fw_url` |
| `destination` | glob on the E.164 number, e.g. `9198100*` | `fw_destination` |

**SMS pumping / SIM-farm guard and flood protection** (per account; `PUT /v1/accounts/{id}/firewall-limits` sets them, and 0 turns a limit off):

| Limit | Default | Reason |
| --- | --- | --- |
| Messages to one number per hour | 50 | `fw_destination_rate` |
| Messages into one 1,000-number range per 10 minutes | 300 | `fw_series` |
| How long a range that trips stays blocked for the account | 60 minutes | `fw_series` |

Fraudsters trigger OTPs to number ranges they hold on SIM farms and are paid per message delivered. Genuine traffic is spread across ranges; pumping concentrates on a few.
- **When a range trips:** it is blocked for that account. The edge (smpp-server or http-api) logs `number range blocked for suspected SMS pumping`.
- **Seeing and clearing blocks:** `GET /v1/accounts/{id}/firewall/blocks` lists blocked ranges. `DELETE …/firewall/blocks/{range}` reopens one and resets its count, for traffic found to be genuine.
- **Memory:** counters are one Redis hash per range per hour (both limits), so memory stays proportional to the ranges an account sends to, not to every number.

**Codes:**

| Reason | HTTP 422 `code` | SMPP status |
| --- | --- | --- |
| fw_sender | fw_sender | 0x420 |
| fw_content | fw_content | 0x421 |
| fw_url | fw_url | 0x422 |
| fw_destination | fw_destination | 0x423 |
| fw_destination_rate | fw_destination_rate | 0x424 |
| fw_series | fw_series | 0x425 |

## Migration

`postgres/0012_failover_hlr_firewall.sql` adds:
- failover policy columns on `vendors` (failover on by default);
- `number_series` and `mnp_imports`;
- `hlr` / `hlr_reject` on `routes`;
- `firewall_rules` and `firewall_limits`.

New Kafka topic: `sms.reroute`, in `deploy/kafka-topics.sh` and created automatically in dev. The load generator now spreads destinations across the number range like real traffic; sequential numbers would trip the pumping guard.

## Verification

- **Unit tests:**
  - failover verdicts: every signal, minimum samples, cooldown, immediate recovery on binds, unchanged "since";
  - submit recorder;
  - routing: down vendors skipped in LCR and priority, the avoid list, fallbacks when nothing is healthy;
  - connector: reroute when down, on temporary errors and on transport errors; no reroute for vendor refusals or HTTP 4xx, or after 2 reroutes;
  - firewall rule matching.
- **Integration tests (Redis/Postgres):**
  - health evaluation and publication, and stale instances cleaned up;
  - series longest prefix, MNP import, resolve order, and Redis outage falling back to series;
  - HLR client: auth, MCC/MNC parsing, statuses, cache (unknown answers not cached), HTTP errors;
  - velocity limits, range blocking, unblocking and reset, and recovery after Redis loses its scripts;
  - admin endpoints.
- **End to end on the dev stack:**
  - failover drill (above);
  - a ported number routed to the vendor cheaper on its new network; an HLR-invalid number refused (`HLR_INVALID`); HLR answered from cache on repeat;
  - firewall content rule, per-number limit, range block with unblock, each with the right code.
- **Load with DLT, MNP and firewall on** (one 4 vCPU box):

  | Run | Accepted TPS | p50 / p95 / p99 | Before this slice |
  | --- | --- | --- | --- |
  | 1,500 TPS | ~1,494 | 22–30 / 69–86 / 105–123 ms | 18 / 63 / 102 ms |
  | 2,500 TPS | 2,461 | 80 / 221 / 280 ms | 40 / 201 / 285 ms |

  - 0 failed submits, all receipts back; `billing-check` passed.
  - The first version created a Redis key per destination per hour (833,000 keys after a few runs). The per-range hash and the shared MNP + firewall round trip fixed that and the extra latency.
