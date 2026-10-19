// Command rater prices messages and writes ledger entries to PostgreSQL in
// batches (blueprint rule 4: reserve in Redis, settle in batches).
package main

import (
	"context"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

func main() {
	service.Run("rater", service.Needs{Postgres: true, Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		return service.Consume(ctx, d, "rater", []string{kafka.TopicBillingEvents}, func(ctx context.Context, rec *kgo.Record) error {
			// TODO(phase 2, blueprint "Wallet and credit limit"): settle holds via
			// wallet.Store and append ledger_entries once a second, keyed by
			// idempotency key so replays never double-charge.
			d.Log.Debug("billing event", "partition", rec.Partition, "offset", rec.Offset)
			return nil
		})
	})
}
