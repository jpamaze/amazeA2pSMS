// Command router picks a vendor for each accepted message: DLT checks,
// firewall, HLR/MNP lookup and least-cost routing, all from in-memory config.
package main

import (
	"context"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

func main() {
	service.Run("router", service.Needs{Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		return service.Consume(ctx, d, "router", []string{kafka.TopicSubmit}, func(ctx context.Context, rec *kgo.Record) error {
			// TODO(phase 1, blueprint "Architecture"): decode domain.Message, run DLT
			// template match, firewall and route selection, then produce to
			// kafka.RouteTopic(vendorID) or release the hold and emit a rejection.
			d.Log.Debug("submit received", "partition", rec.Partition, "offset", rec.Offset)
			return nil
		})
	})
}
