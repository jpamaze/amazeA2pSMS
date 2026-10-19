// Command connector holds outbound SMPP and HTTP binds to one vendor and
// sends routed messages, honouring per-bind TPS, window and SMPP version.
// DLRs are written to Kafka before deliver_sm_resp (blueprint rule 3).
package main

import (
	"context"
	"fmt"

	"github.com/caarlos0/env/v11"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

type connectorConfig struct {
	VendorID string `env:"VENDOR_ID,required"`
}

func main() {
	var cc connectorConfig
	service.Run("connector", service.Needs{Postgres: true, Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&cc); err != nil {
			return fmt.Errorf("connector config: %w", err)
		}
		d.Log = d.Log.With("vendor", cc.VendorID)
		group := "connector-" + cc.VendorID
		return service.Consume(ctx, d, group, []string{kafka.RouteTopic(cc.VendorID)}, func(ctx context.Context, rec *kgo.Record) error {
			// TODO(phase 1): load vendor_binds / http_vendor_profiles, submit_sm or
			// HTTP POST with DLT TLVs from tlv_profile, store vendor msg ID mapping.
			d.Log.Debug("routed message", "offset", rec.Offset)
			return nil
		})
	})
}
