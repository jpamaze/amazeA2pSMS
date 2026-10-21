# Phase 2: billing and commercial

Slice 1 adds reseller billing (two-level wallets), reseller rate cards, wallet transfers, statements and usage reports. Invoices and GST are out of scope for now (statements and usage reports only). Deals and disputes are slice 2: [phase2-deals-disputes.md](phase2-deals-disputes.md).

## Two-level wallets

Every message charges each account in the sender's chain up to, but not including, the platform:

```
platform ── reseller R ── customer C
             charged at     charged at
             platform's     R's price
             price for R    for C
```

- At submit, ingest walks the chain (`internal/billing/hierarchy`, recursive CTE, cached for 1 minute). It prices the message on each account's own rate card and places a hold on each wallet, all with hold ID = message ID.
- The message is refused if any wallet cannot cover its hold. Every hold already placed is released.
- The message is also refused if any account in the chain is not active.
- The charges travel with the message (`domain.Charge` per account) through router, connector and `sms.status`. The rater settles or releases every level in one ledger batch.
- ClickHouse `message_records.charges` is a `Map(account → amount)`, so usage reports and `billing-check` see each level separately.

| Refusal | HTTP | SMPP `command_status` |
| --- | --- | --- |
| Customer wallet short | 402 `insufficient_balance` | `0x401` |
| No rate on the customer's card | 422 `no_rate` | `0x402` |
| Customer or a reseller above it suspended | 403 `account_inactive` | `0x403` |
| Reseller wallet short, or no rate on the reseller's card | 402 `reseller_billing` | `0x404` |

The `reseller_billing` reason is deliberately vague towards the end customer. Their reseller sees the real reason (`reseller_balance` / `reseller_no_rate`) in `sms_refused_total{reason}`.

## Rate cards

- A rate card is owned by the platform (`owner_account_id` NULL) or by a reseller. Names are unique per owner.
- An account can only be given a card owned by its parent. Platform cards go to direct customers and resellers. A reseller's own cards go to its sub-accounts.
- Entries are `(mcc, mnc, category, price, valid_from)`. A future `valid_from` schedules a price change; the newest entry already in effect wins.
- CSV import with header `mcc,mnc,category,price,valid_from`. `valid_from` is optional and RFC 3339.
- Vendor costs (platform only) import the same way and feed margin reporting.

## Transfers

A reseller user (finance role or above) funds one of the reseller's sub-accounts from the reseller's wallet:

```
POST /v1/accounts/{sub-account id}/transfers
Idempotency-Key: <unique>
{"amount": "500.00", "reference": "Oct top-up"}
```

1. The Redis wallet is debited atomically (`debit.lua`). The amount must be covered by balance + credit limit − holds.
2. One PostgreSQL transaction then updates both wallets and writes `transfer_out` / `transfer_in` ledger rows with each other as `counterparty`.
3. The Redis copy of the recipient is credited.

If the PG step fails, or the idempotency key was already used, the debit is refunded. A duplicate returns 409, and a short wallet returns 422. 20 concurrent transfers against a wallet that covers 10 succeed exactly 10 times (`ledger_test.go`).

## Reports

| Endpoint | What |
| --- | --- |
| `GET /v1/accounts/{id}/statement?from=&to=[&format=csv]` | Opening balance, every ledger row (top-ups, transfers with counterparty, charge batches with message count), closing balance |
| `GET /v1/accounts/{id}/usage?from=&to=` | From ClickHouse, per day: messages sent, parts, delivered, not delivered, messages charged to this account that were sent by its sub-accounts (`via_sub_accounts`), and the amount charged *to this account* |

Admin users see their own account and, for resellers, their sub-accounts (`managedOrSelf`).

## Admin API added in this slice

| Method and path | Permission |
| --- | --- |
| `POST /v1/rate-cards` | admin |
| `GET`/`PUT /v1/rate-cards/{card}/entries` | read / admin |
| `POST /v1/rate-cards/{card}/entries/import` (CSV) | admin |
| `POST /v1/vendors/{vendor}/costs/import` (CSV, platform only) | admin |
| `POST /v1/accounts/{id}/transfers` | finance |
| `GET /v1/accounts/{id}/statement` | read |
| `GET /v1/accounts/{id}/usage` | read |

## Migrations

- `postgres/0009_reseller_billing.sql`: `rate_cards.owner_account_id`; ledger kinds `transfer_in` / `transfer_out`; `ledger_entries.quantity` and `counterparty`.
- `clickhouse/0002_charges.sql`: `message_records.charges`.

Both apply automatically (`migrate`, `ch-writer`). Old in-flight messages without `charges` settle through the single-hold fallback in the rater.

## Verification

- `go test -race ./...` passes three times in a row. Tests run in a Postgres schema per test (`internal/platform/pgtest`), so packages run in parallel safely.
- Local end to end with reseller R (₹100) and shop S (₹10). R prices S at ₹0.20 and the platform prices R at ₹0.12. 30 messages leave S at ₹4.00 and R at ₹86.40 (₹100 − ₹10 transfer − 30 × ₹0.12). Other checks:
  - A duplicate transfer gets 409 and an overdraft gets 422.
  - R running dry gets 402 `reseller_billing`.
  - Assigning a card R does not own gets 400.
  - The statement and usage totals match, and `billing-check` passes.
- Load regression on one 4 vCPU box (everything on one machine): 2,476 TPS accepted, 0 failures, all 74,332 receipts delivered. `billing-check` passes: there are no open holds, Redis equals PG, and the ledger equals ClickHouse per account.
