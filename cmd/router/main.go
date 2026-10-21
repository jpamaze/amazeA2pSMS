// Command router picks a vendor for each accepted message and forwards it to
// that vendor's queue. Messages with no route are rejected (status event),
// which releases the balance hold. DLT checks, firewall and HLR/MNP lookups
// join this step in phase 3.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/routing"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

func main() {
	service.Run("router", service.Needs{Postgres: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		loader, err := routing.NewLoader(ctx, d.PG, d.Log)
		if err != nil {
			return err
		}
		go loader.Run(ctx, 15*time.Second)

		return service.ConsumeBatch(ctx, d, "router", []string{kafka.TopicSubmit}, 1000, func(ctx context.Context, recs []*kgo.Record) error {
			table := loader.Table()
			out := make([]*kgo.Record, 0, len(recs))
			for _, rec := range recs {
				var m domain.Message
				if err := json.Unmarshal(rec.Value, &m); err != nil {
					d.Log.Error("bad submit record; skipping", "offset", rec.Offset, "err", err)
					continue
				}
				dec, err := table.Choose(m.AccountID, m.MCC, m.MNC)
				if errors.Is(err, routing.ErrNoRoute) {
					metrics.Routed.WithLabelValues("none").Inc()
					r, err := kafka.Record(kafka.TopicStatus, m.ID, domain.StatusEvent{
						MessageID: m.ID, AccountID: m.AccountID, HoldID: m.HoldID, Price: m.Price,
						Status: domain.StatusRejected, ErrorCode: "NO_ROUTE", At: time.Now().UTC(),
					})
					if err != nil {
						return err
					}
					out = append(out, r)
					continue
				}
				m.Route = &domain.Route{VendorID: dec.VendorID, Cost: dec.Cost}
				metrics.Routed.WithLabelValues(dec.VendorID).Inc()
				r, err := kafka.Record(kafka.RouteTopic(dec.VendorID), m.ID, m)
				if err != nil {
					return err
				}
				out = append(out, r)
			}
			return kafka.ProduceJSON(ctx, d.Kafka, out...)
		})
	})
}
