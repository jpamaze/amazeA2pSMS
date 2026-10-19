# ADR 0001: Go services around Kafka, Redis, PostgreSQL and ClickHouse

Status: accepted

## Decision

- Go for every service on the message path. One node handles thousands of SMPP connections and 5,000+ TPS.
- Kafka (Redpanda in development) as the durable queue between every step. Customers are acknowledged only after acks=all.
- Redis for hot state: balance holds (Lua scripts, no row locks), rate limits, vendor message ID mapping.
- PostgreSQL 16 for accounts, configuration, DLT registrations and the ledger.
- ClickHouse for message records and reports (about 45 GB/day compressed at 5,000 TPS, 90 days online).
- Next.js for the portals (later phase).

## Why

The previous Node.js codebase stalled at 100–500 TPS: about 25 uncached database queries per message, a balance row locked twice per message, and the vendor call made inside the HTTP request. This design removes all three.

## Consequences

- The team needs Go and Kafka operations skills. Use managed Kafka and ClickHouse in year one.
- Every handler must be idempotent, because Kafka delivery is at-least-once.
