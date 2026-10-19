// Command rater settles balance holds and writes charges to the ledger in
// batches (blueprint rule 4: reserve in Redis, settle in batches).
package main

import (
	"context"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/rater"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

func main() {
	service.Run("rater", service.Needs{Postgres: true, Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		r := &rater.Rater{Wallets: wallet.New(d.Redis), PG: d.PG, Log: d.Log}
		return service.ConsumeBatch(ctx, d, "rater", []string{kafka.TopicStatus}, 5000,
			func(ctx context.Context, recs []*kgo.Record) error { return r.Process(ctx, recs) })
	})
}
