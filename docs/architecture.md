# Architecture

The full blueprint (design targets, stack, capacity, roadmap) is the "New SMS Platform Blueprint" doc. This file holds the parts the code depends on.

## Message path

```
Customer SMPP ─▶ smpp-server ─┐  hold + state in Redis, then
Customer HTTP ─▶ http-api ────┴─▶ sms.submit ─▶ router ─▶ sms.route.<vendor> ─▶ connector ─▶ vendor
                                                  │ no route                      │ submitted / failed
                                                  ▼                               ▼
                                              sms.status ◀────────────────────────┘
                                                  │
                         ┌────────────────────────┴───────────────────────┐
                         ▼                                                ▼
                 rater: settle/release hold,                    dlr-processor: message state
                 ledger batch to PostgreSQL                     ▲            │ final status
                                                                │            ▼
vendor deliver_sm ─▶ connector ─▶ sms.dlr ──────────────────────┘   custdlr:{account} stream (Redis)
                                                                             │
                                               smpp-server ◀─────────────────┘ deliver_sm to RX/TRX binds
```

| Service | Reads | Writes | Stores |
| --- | --- | --- | --- |
| smpp-server | customer binds, `custdlr:{account}` | `sms.submit` | Redis (holds, state, receipts), PG (credentials, prices, DLT registrations) |
| http-api | REST | `sms.submit` | Redis, PG |
| admin-api | REST; serves the portal (static export) | — | PG, Redis, ClickHouse (reports, message search), Prometheus (live monitor) |
| alerter | — (polls every 30 s) | e-mail (SMTP), SMS (through http-api), webhooks | PG (alert settings and events), Redis (wallets) |
| router | `sms.submit`, `sms.reroute` | `sms.route.<vendor>`, `sms.status` (rejected) | in-memory routes, costs and vendor deals, refreshed from PG; Redis (vendor deal progress, failover verdicts, HLR cache); HLR provider |
| connector | `sms.route.<vendor>`, vendor deliver_sm | `sms.status`, `sms.dlr`, `sms.mo`, `sms.reroute` (failover) | Redis (vendor msg ID map), PG (binds) |
| dlr-processor | `sms.status`, `sms.dlr` | `custdlr:{account}`, `sms.webhooks`, `sms.records` | Redis; PG (vendor error codes, reloaded every minute) |
| rater | `sms.status` | — | Redis wallet and deal reservations, PG ledger + `rater_offsets` + deal usage |
| webhook-dispatcher | `sms.webhooks`, `sms.webhooks.retry.1-4` | retry tiers, `sms.webhooks.dead` | PG (webhook URL, encrypted secret) |
| ch-writer | `sms.submit`, `sms.records` | ClickHouse `message_records` | — |

HTTP vendors: the connector POSTs per `http_vendor_profiles` and serves `/dlr/<vendor>?token=…` on `DLR_ADDR`; receipts go to `sms.dlr` exactly like SMPP ones.

Redis keys: `wallet:{acct}` and `wallet:{acct}:holds` (balance, credit limit, holds), `msg:{id}` (state, 96 h TTL), `vmid:<vendor>:<id>` (DLR match), `inflight:<0-15>` (submitted, waiting for DLR), `custdlr:{acct}` (receipt stream, consumer group `deliver`), `idem:{acct}:<key>` (HTTP idempotency), `binds:{system_id}` (cross-node bind leases), `deal:{id}` and `deal:{id}:res` (customer deal volume used and reserved per message), `vdeal:{id}` (segments routed under a vendor deal), `vhealth` / `vstate` / `vdlr:{vendor}:<minute>` (failover reports, verdicts, receipt counts), `mnp:<8 digits>` (ported numbers), `hlr:<msisdn>` (HLR cache), `fw:{acct}:h:<range>:<hour>` and `fw:{acct}:b:<range>` (firewall counters and range blocks).

Final receipts (deliver_sm or webhook) and the ClickHouse row are queued before the message is marked notified, so a crash repeats rather than loses them; customers should treat receipts and webhooks as at-least-once and de-duplicate by message ID.

Submit checks, in order: number, text, DLT ([phase3-dlt.md](phase3-dlt.md)), firewall rules, then one Redis round trip for MNP and firewall velocity ([phase3-network.md](phase3-network.md)), then pricing and holds.

Billing: every account in the sender's chain below the platform is charged (two-level wallets; see [phase2-billing.md](phase2-billing.md)). Each is charged when the vendor accepts a message (`submitted`, or `failed` after at least one part was accepted) and the hold is released when the platform rejects it or no part was accepted.

## Reliability rules

1. Write first, then confirm: `submit_sm_resp` / HTTP 202 only after Kafka acks=all (`kafka.NewProducer`).
2. No vendor call inside a customer request.
3. Save the DLR to Kafka before `deliver_sm_resp`.
4. Reserve balance in Redis (`billing/wallet`), settle in batches.
5. Every message has an idempotency key (`kafka.SubmitKey`, `kafka.DLRKey`); every handler is idempotent.
6. Config is read from memory, refreshed from `sms.config.changes`; never queried per message.
7. Per-bind limits for customer and vendor binds (`smpp_credentials`, `vendor_binds`).
8. Automatic vendor failover on errors, latency or DLR rate.
9. Reconciliation: messages still in `submitted` after `DLR_TIMEOUT` are expired and the customer gets an EXPIRED receipt.
10. No single points of failure; `/readyz` checks every dependency (`platform/health`).

## Ops endpoints

Every service serves on `OPS_ADDR` (default `:9090`): `/healthz` (process up), `/readyz` (all dependencies reachable, else 503), `/metrics` (Prometheus).
