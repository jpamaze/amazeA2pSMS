package service

import (
	"context"
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
)

// Handler processes one record. Returning an error stops the consumer without
// committing, so the record is redelivered after restart (at-least-once;
// handlers must be idempotent, blueprint rule 5).
type Handler func(ctx context.Context, rec *kgo.Record) error

// Consume runs a group consumer on topics until ctx is cancelled. Offsets are
// committed only after every record in a poll has been handled.
func Consume(ctx context.Context, d *Deps, group string, topics []string, h Handler) error {
	cl, err := kafka.NewConsumer(d.Cfg.KafkaBrokers, group, topics)
	if err != nil {
		return err
	}
	defer cl.Close()
	d.Health.Add("kafka-consumer", kafka.Ping(cl))
	d.Log.Info("consuming", "group", group, "topics", topics)

	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, e := range errs {
				d.Log.Warn("fetch error", "topic", e.Topic, "partition", e.Partition, "err", e.Err)
			}
			continue
		}
		var herr error
		fetches.EachRecord(func(rec *kgo.Record) {
			if herr == nil {
				herr = h(ctx, rec)
			}
		})
		if herr != nil {
			return fmt.Errorf("handler: %w", herr)
		}
		if err := cl.CommitUncommittedOffsets(ctx); err != nil && ctx.Err() == nil {
			d.Log.Warn("commit failed", "err", err)
		}
	}
}
