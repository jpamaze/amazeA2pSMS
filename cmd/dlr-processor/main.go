// Command dlr-processor matches vendor DLRs to messages, updates records and
// pushes DLRs and MO messages to customers (SMPP deliver_sm or webhook).
package main

import (
	"context"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

func main() {
	service.Run("dlr-processor", service.Needs{Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		return service.Consume(ctx, d, "dlr-processor", []string{kafka.TopicDLR, kafka.TopicMO}, func(ctx context.Context, rec *kgo.Record) error {
			// TODO(phase 1): look up vendor msg ID -> message ID in Redis, write the
			// status change to ClickHouse, settle or release the hold, and deliver
			// to the customer.
			d.Log.Debug("dlr/mo received", "topic", rec.Topic, "offset", rec.Offset)
			return nil
		})
	})
}
