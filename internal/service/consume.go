package service

import (
	"context"
	"fmt"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"

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

// RetryFor is how long a failing batch is retried in place before the
// consumer gives up and the service exits (to be restarted, and the batch
// redelivered). It covers a PostgreSQL or Redis failover (about 30 s and
// 10 s); handlers are idempotent, so a retry is the same as a redelivery.
var RetryFor = 2 * time.Minute

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
	// One check per group: a process can run several consumers (the
	// connector runs one per vendor) and stop some of them.
	check := "kafka-consumer:" + group
	d.Health.Add(check, kafka.Ping(cl))
	defer d.Health.Remove(check)
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
			start := time.Now()
			if err := retry(ctx, d, group, func() error { return h(ctx, recs) }); err != nil {
				cl.AllowRebalance()
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("handler: %w", err)
			}
			metrics.BatchSeconds.WithLabelValues(group).Observe(time.Since(start).Seconds())
			metrics.BatchRecords.WithLabelValues(group).Observe(float64(len(recs)))
			if err := cl.CommitRecords(ctx, recs...); err != nil && ctx.Err() == nil {
				d.Log.Warn("commit failed", "err", err)
			} else {
				metrics.ConsumedRecords.WithLabelValues(group).Add(float64(len(recs)))
			}
		}
		cl.AllowRebalance()
	}
}

// retry runs fn until it succeeds, ctx ends or RetryFor has passed, with
// backoff from 250 ms to 5 s.
func retry(ctx context.Context, d *Deps, group string, fn func() error) error {
	deadline := time.Now().Add(RetryFor)
	wait := 250 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil {
			if attempt > 1 {
				d.Log.Info("batch succeeded after retrying", "group", group, "attempts", attempt)
			}
			return nil
		}
		if ctx.Err() != nil || time.Now().Add(wait).After(deadline) {
			return err
		}
		metrics.BatchRetries.WithLabelValues(group).Inc()
		d.Log.Warn("batch failed; retrying", "group", group, "attempt", attempt, "in", wait, "err", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
		wait = min(wait*2, 5*time.Second)
	}
}
