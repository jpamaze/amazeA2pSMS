// Command dlr-processor matches vendor DLRs to messages, updates message
// state and queues delivery receipts for customers.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/dlr"
	"github.com/jpamaze/amazeA2pSMS/internal/dlt"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

type dlrConfig struct {
	// Messages with no DLR after this long are marked expired. Must be below
	// msgstore.TTL (96h).
	DLRTimeout     time.Duration `env:"DLR_TIMEOUT" envDefault:"72h"`
	ReconcileEvery time.Duration `env:"RECONCILE_EVERY" envDefault:"1m"`
}

func main() {
	var dc dlrConfig
	service.Run("dlr-processor", service.Needs{Postgres: true, Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&dc); err != nil {
			return fmt.Errorf("dlr-processor config: %w", err)
		}
		codes := &dlt.ErrorMap{PG: d.PG, Log: d.Log}
		if err := codes.Load(ctx); err != nil {
			return fmt.Errorf("vendor error codes: %w", err)
		}
		go codes.Run(ctx, time.Minute)
		p := &dlr.Processor{Store: msgstore.New(d.Redis), Kafka: d.Kafka, Log: d.Log, Errors: codes}
		go p.Reconcile(ctx, dc.DLRTimeout, dc.ReconcileEvery)
		return service.ConsumeBatch(ctx, d, "dlr-processor", []string{kafka.TopicStatus, kafka.TopicDLR}, 2000,
			func(ctx context.Context, recs []*kgo.Record) error { return p.Process(ctx, recs) })
	})
}
