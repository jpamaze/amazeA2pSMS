# Phase 2 slice 2: deals and disputes

Slice 2 covers both sides of each: customer and vendor deals, customer and vendor disputes. Slice 1 (reseller billing, rate cards, transfers, reports) is in [phase2-billing.md](phase2-billing.md).

## Customer deals

A deal is a special price for one account on one destination (MCC, with one network or `*`) for a period. A deal can optionally carry a volume cap in segments. After the cap is used up, the rate card applies again.

- **Who grants it:** only the account's parent. The platform grants deals to its direct customers and resellers; a reseller grants them to its sub-accounts. Two active deals for the same account and destination may not overlap in time (409).
- **Which price applies:** at submit, each paying level in the chain (sender, then its resellers) is priced by its own running deal if there is one, otherwise by its rate card. A deal for an exact network beats a `*` deal. So the platform can give a reseller a deal, and the reseller can give its sub-account a different deal on the same message.
- **Cap:** volume is reserved in Redis per message (`deal:{id}`, `deal:{id}:res`), atomically and idempotently, the same way as balance holds.
  - The reservation is kept when the message is charged and given back when it is released (rejected, no route, never accepted by a vendor).
  - A message that doesn't fit entirely in the remaining volume is priced at the card rate as a whole; parts are never split between prices.
- **Used volume:** the rater adds charged segments to `customer_deals.used_segments` in the same transaction as the ledger batch, so it is exactly once. If Redis loses the counter, it is reseeded from that value.
- **Cost:** a capped deal costs one extra Redis call per message while it has volume. Once it is used up, each edge remembers that and stops asking Redis until its cache refreshes (at most 1 minute).
- **Changes take effect** within 1 minute (the edge cache), including cancellation.

## Vendor deals

A vendor gives us a lower cost on a destination for a period in return for committed volume, and may charge `shortfall_rate` per segment we fall short.

- **Cost:** while the deal runs, its cost replaces the vendor's cost list for that destination. This applies in every routing mode, so the recorded margin is right.
- **Preference:** in least-cost routing, vendors with an *unmet* commitment are chosen first (cheapest among them). After that, plain least-cost applies again.
- **Progress for routing:** each router counts segments it routes under a deal into Redis (`vdeal:{id}`) and refreshes the totals every 15 seconds. This count only steers routing.
- **Progress for reporting:** the report counts from ClickHouse, using messages the vendor actually accepted.
- **Report** (`GET /v1/vendor-deals/{id}`):
  - billed segments and cost, percent met, days left, and the daily volume needed;
  - after one full day, a straight-line projection with the projected shortfall charge.

## Customer disputes

- **Raising:** an account raises a dispute for itself. It gives the period, optional message IDs (up to 1,000), the amount claimed and a reason.
- **Resolving:** the account's parent resolves it. The platform resolves disputes from its direct customers and resellers; a reseller resolves disputes from its sub-accounts.
- **Evidence:** `GET /v1/disputes/{id}` attaches evidence from ClickHouse:
  - the account's messages in the period by final status, with segments and the amount charged to it;
  - for each listed message ID: its status, error, vendor and charge. IDs we have no record of are listed separately.
- **Approval:** an approval names an amount up to the claim and is credited at once.
  - **Platform resolver:** the credit is a `refund` ledger entry.
  - **Reseller resolver:** the credit comes out of the reseller's own wallet (`refund_given` on the reseller, `refund` on the sub-account, each with the other as counterparty). If the reseller's available balance can't cover it, the API returns 422 and the dispute stays open.
- **Safety:** two approvals can't both pay. The dispute is claimed with a conditional update first, and the credit's idempotency key is `dispute:<id>`.
- **Other outcomes:** reject (with a note) or withdraw (by the claimant).

## Vendor invoices and disputes

1. **Import the invoice:** `POST /v1/vendors/{vendor}/invoices?reference=INV-123&from=2026-10-01&to=2026-10-31` with CSV `day,mcc,mnc,segments,amount`. Days are IST and `mnc` may be `*` for country-level billing. The response includes the reconciliation.
2. **Reconcile:** `GET /v1/vendor-invoices/{id}/reconciliation?tolerance_pct=0.5` compares each line with our records for that vendor, IST day and destination.
   - Our side counts messages the vendor accepted: submitted, delivered or expired, or failed with a vendor message ID. Cost is the vendor cost recorded per message.
   - Country-level vendor lines are compared against our networks summed.
   - Line statuses: `ok`, `mismatch` (outside tolerance on segments or amount), `not_in_ours` (billed, but we have no record) and `not_in_vendor` (we sent it, they didn't bill it).
   - Disputable amount: what the vendor billed above our figure on `mismatch` and `not_in_ours` lines. Underbilling is shown but never disputed.
3. **Open a dispute:** `POST /v1/vendor-invoices/{id}/disputes` opens one with a snapshot of those lines. There is at most one open (or sent) dispute per invoice.
4. **Track it:** `PATCH /v1/vendor-disputes/{id}` moves it along `open → sent → accepted | rejected → settled`. `settled` requires `settled_amount` (the credit note received, at most the disputed amount). Every step's note is appended to the dispute's trail.

## Admin API added in this slice

| Method and path | Who | Permission |
| --- | --- | --- |
| `POST /v1/accounts/{id}/deals` | the account's parent | admin |
| `GET /v1/accounts/{id}/deals` | the account and its managers | read |
| `POST /v1/deals/{id}/cancel` | the granter | admin |
| `POST /v1/vendors/{vendor}/deals`, `GET /v1/vendor-deals`, `GET /v1/vendor-deals/{id}`, `POST /v1/vendor-deals/{id}/cancel` | platform | admin / read |
| `POST /v1/accounts/{id}/disputes` | the account itself | finance |
| `GET /v1/disputes[?status=]`, `GET /v1/disputes/{id}` | claimant and resolver | read |
| `POST /v1/disputes/{id}/resolve` (`approve` with amount, or `reject`; note required) | the resolver | finance |
| `POST /v1/disputes/{id}/withdraw` | the claimant | finance |
| `POST /v1/vendors/{vendor}/invoices` (CSV), `GET /v1/vendor-invoices/{id}/reconciliation` | platform | finance / read |
| `POST /v1/vendor-invoices/{id}/disputes`, `GET /v1/vendor-disputes`, `PATCH /v1/vendor-disputes/{id}` | platform | finance / read |

Every change is recorded in `admin_audit` like the rest of the admin API.

## Migration

`postgres/0010_deals_disputes.sql` adds `customer_deals`, `vendor_deals`, `customer_disputes`, `vendor_invoices`, `vendor_invoice_lines` and `vendor_disputes`. It also adds the `refund_given` ledger kind. The router now needs Redis (`REDIS_ADDRS`) for vendor deal progress.

## Fixed along the way

ClickHouse timestamps were written as UTC wall-clock time into `Asia/Kolkata` columns, so every stored time was 5½ hours early. Day boundaries in usage reports were shifted, and time windows only matched because the queries made the same mistake.
- `ch-writer` now writes IST.
- Queries pass UTC explicitly (usage, `billing-check`, the new reports).
- Rows written before this fix stay shifted. That only matters for dev or test data; nothing is in production yet.

## Verification

- **Unit and integration tests** (`go test -race ./...`, run three times):
  - routing with vendor deals: an unmet commitment wins even when dearer, it releases once met, a network deal beats a country deal, and an ended deal is ignored;
  - deal reservations: 30 concurrent 5-segment messages against a cap of 100 reserve exactly 20; reserve and release are idempotent; settled volume stays used; the counter is seeded from PostgreSQL; a used-up deal is skipped;
  - reconciliation lines and disputable amount;
  - the full admin flow: granting rules, overlap, platform-only vendor deals, reseller- and platform-paid dispute credits with Redis and PostgreSQL balances matching, the reseller-short 422 that leaves the dispute open, double approval refused, and vendor dispute transitions;
  - commitment projection.
- **End to end on the local stack** (smpp-sim at ₹0.05, httpsim at ₹0.06):
  - A capped deal (₹0.10 for 25 segments; card ₹0.12) gave 25 messages at ₹0.10 and 15 at ₹0.12. The charge was ₹4.30 and `used_segments` 25.
  - A httpsim deal (₹0.055, commit 20) took the first 20 messages; the other 20 went to sim. Its report showed 20 billed segments, ₹1.10 cost and 100% met.
  - An invoice for 25 segments / ₹1.375 reconciled as a mismatch with ₹0.275 disputable, and the dispute opened.
  - A customer dispute showed both listed messages with vendor, status and charge.
  - `billing-check` passed.
- **Load on one 4-vCPU box with everything on it:**

  | Run | Accepted TPS | p50 / p95 / p99 | Notes |
  | --- | --- | --- | --- |
  | 1,500 TPS, capped deal with volume | 1,494 | 25.7 / 68.6 / 100 ms | |
  | 1,500 TPS, no deal | 1,486 | 17.3 / 52.1 / 84.1 ms | baseline |
  | 2,500 TPS, capped deal with volume | 2,458 | 58.6 / 179.3 / 301.7 ms | 0 failures, all receipts back |

  After the runs, deal usage matched in PostgreSQL and Redis (118,811 segments) with no reservations left open, and `billing-check` passed.
