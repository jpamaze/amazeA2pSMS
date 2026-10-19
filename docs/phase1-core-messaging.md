# Phase 1: Core messaging, slice 1

Slice 1 is a working path end to end: a customer submits over SMPP or HTTP, the message goes to a vendor over SMPP, and the delivery receipt comes back to the customer, with the balance held and charged correctly.

## What is built

| Area | What works | Code |
| --- | --- | --- |
| SMPP library | Own PDU codec for SMPP 3.3/3.4/5.0 (bind ×3, submit_sm, deliver_sm, enquire_link, unbind, generic_nack); TLVs on 3.4+; long text moves to `message_payload`; Appendix B receipt format and parsing (TLVs preferred) | `internal/smpp/pdu` |
| SMPP sessions | Request windowing, sequence matching, response timeouts, enquire_link keepalive, idle detection, unbind, generic_nack for unknown or malformed PDUs, bounded concurrent handlers | `internal/smpp/session` |
| Encoding | GSM 03.38 (with extension table), UCS-2, segment counting (160/153, 70/67), splitting that never breaks an escape or surrogate pair | `internal/smpp/gsm7` |
| Customer SMPP | bcrypt bind auth, IP allowlist, max binds per system_id (per node), per-bind TPS limit (answers 0x58 when exceeded), interface version negotiation, DLT fields from TLVs (0x1400/0x1401/0x1402 by default, set by env), UDH pass-through, receipts pushed to RX/TRX binds | `internal/edge/smppserver` |
| Customer HTTP | `POST /v1/sms`, `GET /v1/sms/{id}`, `GET /v1/wallet`; API keys (`amz_<prefix>_<secret>`, SHA-256 stored); Idempotency-Key; per-account rate limit | `internal/edge/httpapi`, `api/openapi.yaml` |
| Intake | India number normalisation, sender check, pricing from the rate card (cached), wallet hold, Redis state, Kafka acks=all before the customer is answered; every failure rolls back the hold | `internal/ingest` |
| Routing | LCR, priority and weighted rules, per-account rules before platform defaults, inactive vendors skipped, table reloaded every 15 s | `internal/routing`, `cmd/router` |
| Vendor connector | Several binds per vendor with reconnect and backoff, per-bind TPS and window, concatenation with 8-bit UDH, DLT TLVs from `tlv_profile`, retries on throttling, queue-full and timeouts, encrypted bind passwords, skips a replayed message that was already sent | `internal/connector`, `cmd/connector` |
| DLR journal | Vendor `deliver_sm` written to Kafka before `deliver_sm_resp`; vendor redelivers if Kafka is down | `connector.Deliver` |
| DLR processing | Vendor ID → message match (re-queued up to 2 min if not visible yet), multi-part aggregation, duplicate DLRs counted once, monotonic status, exactly one customer receipt | `internal/dlr`, `internal/msgstore` |
| Reconciliation | Messages with no DLR after `DLR_TIMEOUT` (72 h by default) become `expired`, and the customer gets an EXPIRED receipt | `dlr.Processor.Reconcile` |
| Billing | Charged when the vendor accepts, released when rejected or never sent; Redis settle is idempotent; PostgreSQL ledger written in batches exactly once (`rater_offsets`) | `internal/billing/rater` |
| Dev tooling | `devkafka` (in-memory Kafka), `smpp-sim` (vendor SMSC), `smpp-load` (customer load generator), `devseed`, `scripts/dev-local.sh` | `cmd/*` |

## Test results

All of these ran on one 4-vCPU, 15 GB container, with the in-memory Kafka, Redis, PostgreSQL, all seven services, the simulator and the load generator sharing it.

| Test | Result |
| --- | --- |
| 1,000 TPS, 20 s | 20,012 accepted, 0 failed; submit_sm_resp p50 3.5 ms / p99 17 ms; every receipt back 0.4 s after sending stopped |
| 3,000 TPS, 20 s | 59,969 accepted, 0 failed; p50 11 ms / p99 48 ms; every receipt back |
| 5,000 TPS, 30 s | 145,929 accepted (4,859 TPS), 0 failed; p50 34 ms / p99 259 ms (box saturated); every receipt back, the last 15 s after sending stopped |
| Vendor killed for 8 s at 500 TPS | 0 messages lost; queued while the vendor was down and sent on reconnect; receipts the vendor never sent were expired by reconciliation |
| Connector `kill -9` at 1,000 TPS | 0 lost; 15 duplicate submits to the vendor (in flight at the crash); every receipt back about 1 s after sending stopped |
| Billing after a crash | Ledger total = messages × price exactly; Redis and PostgreSQL balances equal; no holds left open |
| Balance / credit | Rs 1 wallet at Rs 0.12: 8 accepted, then 0x401; after a Rs 1 credit limit, 8 more accepted |

Customer SMPP error codes: 0x58 throttled, 0x0B invalid destination, 0x0A invalid sender, 0x01 empty or too long, 0x104 unsupported data_coding, 0x14 temporarily unavailable (retry), **0x401 insufficient balance**, **0x402 no rate for destination** (0x400–0x4FF are vendor-specific per the SMPP spec).

## Not in this slice

| Item | Plan |
| --- | --- |
| HTTP vendor connector | Next slice: `http_vendor_profiles` request and response mapping, the same retry and journal rules |
| HTTP customer webhooks | Next slice: signed callbacks with retries |
| ClickHouse writer | Next slice: status and DLR events to `message_records` |
| Wallet loading for new accounts and top-ups | Next slice: admin-api top-up endpoint writes the ledger and syncs Redis (today only `devseed` loads wallets) |
| Max binds across nodes | Per node now; move to Redis |
| Vendor failover to another vendor | The connector waits while its vendor is down; the router will route around unhealthy vendors (phase 3) |
| DLT template matching, firewall, HLR/MNP | Phase 3 (fields are already carried end to end) |
| Load-test gate on production-shaped hardware | Run `smpp-load` against separate nodes with real Kafka (3 brokers) |
