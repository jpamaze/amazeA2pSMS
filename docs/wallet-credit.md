# Wallet and credit limit

There is no postpaid billing. An account can send while

```
available = wallet balance + credit limit - open holds
```

covers the message cost. Every new account starts with a credit limit of **0**, so it is prepaid until someone raises the limit.

| Rule | Behaviour | Where enforced |
| --- | --- | --- |
| Default credit limit | 0 for every new account | `wallets.credit_limit DEFAULT 0`, wallet row created by trigger on `accounts` |
| Who can change it | Platform admins: any account. Resellers: only their direct sub-accounts | `internal/billing/credit` |
| Reseller cap | Sum of a reseller's sub-account credit limits ≤ reseller balance + reseller credit limit | `wallets_credit_limit_rules` trigger (`migrations/postgres/0002_wallet.sql`), with the reseller's wallet row locked |
| Audit | Every change is logged with old/new limit, who made it and why. A change without an actor and a reason is refused | same trigger, `credit_limit_changes` table |
| Wallet below zero | Allowed down to minus the credit limit; shown as credit used | `hold.lua` |
| Send check | One atomic Redis Lua script; concurrent holds never overspend | `internal/billing/wallet` |
| Top-up | Adds to the balance, so credit used is repaid first | `wallet.Store.TopUp` |
| Lowering below credit used | Allowed; new holds fail until a top-up brings available above zero | `wallet` tests |
| Takes effect | The next hold after the commit (Redis copy updated by `credit.Service`) | `credit.Service.SetLimit` |

Amounts are stored with 4 decimal places (`numeric(18,4)` in PostgreSQL, int64 units of 1/10,000 in Redis).

Redis is the hot copy that holds go through. PostgreSQL is the system of record: the rater settles into `ledger_entries` in batches.

Resellers have their own wallet, charged at the platform's price for every message their sub-accounts send, and fund sub-accounts by transfer. See [phase2-billing.md](phase2-billing.md).
