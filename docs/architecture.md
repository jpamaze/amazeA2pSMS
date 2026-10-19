# Architecture

The full blueprint (design targets, stack, capacity, roadmap) is the "New SMS Platform Blueprint" doc. This file holds the parts the code depends on.

## Message path

```
Customer SMPP ─▶ smpp-server ─┐                         ┌─▶ router ──▶ sms.route.<vendor> ─▶ connector ─▶ vendor
Customer HTTP ─▶ http-api ────┴─▶ sms.submit (Kafka) ───┤
                                                        └─ (hold placed in Redis before produce)
vendor DLR/MO ─▶ connector ─▶ sms.dlr / sms.mo ─▶ dlr-processor ─▶ customer (deliver_sm / webhook)
holds, settles ─▶ sms.billing.events ─▶ rater ─▶ PostgreSQL ledger (batched)
all records ─▶ ClickHouse message_records
```

| Service | Reads | Writes | Stores |
| --- | --- | --- | --- |
| smpp-server | customer binds | `sms.submit` | Redis (holds, limits), PG (credentials) |
| http-api | REST | `sms.submit` | Redis, PG |
| admin-api | REST | `sms.config.changes` | PG, Redis |
| router | `sms.submit` | `sms.route.<vendor>`, `sms.billing.events` | Redis (HLR cache), in-memory config |
| connector | `sms.route.<vendor>` | `sms.dlr`, `sms.mo` | Redis (vendor msg ID map) |
| dlr-processor | `sms.dlr`, `sms.mo` | `sms.billing.events` | Redis, ClickHouse |
| rater | `sms.billing.events` | — | PG ledger, Redis wallet |

## Reliability rules

1. Write first, then confirm: `submit_sm_resp` / HTTP 202 only after Kafka acks=all (`kafka.NewProducer`).
2. No vendor call inside a customer request.
3. Save the DLR to Kafka before `deliver_sm_resp`.
4. Reserve balance in Redis (`billing/wallet`), settle in batches.
5. Every message has an idempotency key (`kafka.SubmitKey`, `kafka.DLRKey`); every handler is idempotent.
6. Config is read from memory, refreshed from `sms.config.changes`; never queried per message.
7. Per-bind limits for customer and vendor binds (`smpp_credentials`, `vendor_binds`).
8. Automatic vendor failover on errors, latency or DLR rate.
9. Hourly reconciliation of messages stuck in `submitted` past `vendors.dlr_timeout`.
10. No single points of failure; `/readyz` checks every dependency (`platform/health`).

## Ops endpoints

Every service serves on `OPS_ADDR` (default `:9090`): `/healthz` (process up), `/readyz` (all dependencies reachable, else 503), `/metrics` (Prometheus).
