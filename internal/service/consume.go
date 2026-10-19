package service

import (
	"context"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
)

// Handler processes one record. Returning an error stops the consumer without
// committing, so the record is redelivered after restart (at-least-once;
// handlers must be idempotent, blueprint rule 5).
type Handler func(ctx context.Context, rec *kgo.Record) error

// BatchHandler processes every record from one poll. Offsets are committed
// only after it returns nil.
type BatchHandler func(ctx context.Context, recs []*kgo.Record) error

// Consume runs h on each record, one at a time.
func Consume(ctx context.Context, d *Deps, group string, topics []string, h Handler) error {
	return ConsumeBatch(ctx, d, group, topics, 0, func(ctx context.Context, recs []*kgo.Record) error {
		for _, r := range recs {
			if err := h(ctx, r); err != nil {
				return err
			}
		}
		return nil
	})
}

// ConsumeBatch runs a group consumer on topics until ctx is cancelled.
// maxRecords caps one poll (0 = 500).
func ConsumeBatch(ctx context.Context, d *Deps, group string, topics []string, maxRecords int, h BatchHandler) error {
	if maxRecords <= 0 {
		maxRecords = 500
	}
	cl, err := kafka.NewConsumer(d.Cfg.KafkaBrokers, group, topics)
	if err != nil {
		return err
	}
	defer cl.Close()
	// Polls block rebalances until released; Close waits for that, so release
	// on every exit path or shutdown hangs.
	defer cl.AllowRebalance()
	d.Health.Add("kafka-consumer", kafka.Ping(cl))
	d.Log.Info("consuming", "group", group, "topics", topics)

	for {
		fetches := cl.PollRecords(ctx, maxRecords)
		if ctx.Err() != nil {
			return nil
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, e := range errs {
				d.Log.Warn("fetch error", "topic", e.Topic, "partition", e.Partition, "err", e.Err)
			}
			cl.AllowRebalance()
			time.Sleep(200 * time.Millisecond)
			continue
		}
		recs := fetches.Records()
		if len(recs) > 0 {
			if err := h(ctx, recs); err != nil {
				cl.AllowRebalance()
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("handler: %w", err)
			}
			if err := cl.CommitRecords(ctx, recs...); err != nil && ctx.Err() == nil {
				d.Log.Warn("commit failed", "err", err)
			}
		}
		cl.AllowRebalance()
	}
}
