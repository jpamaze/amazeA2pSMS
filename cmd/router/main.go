// Command router picks a vendor for each accepted message and forwards it to
// that vendor's queue: an HLR lookup first where the route asks for one,
// then the route, skipping vendors failover has taken out. Messages with no
// route (or refused by HLR) are rejected (status event), which releases the
// balance hold.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/deals"
	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/routing"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
	"github.com/jpamaze/amazeA2pSMS/internal/vendorhealth"
)

func main() {
	service.Run("router", service.Needs{Postgres: true, Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		loader, err := routing.NewLoader(ctx, d.PG, d.Redis, d.Log)
		if err != nil {
			return err
		}
		go loader.Run(ctx, 15*time.Second)
		go evaluateHealth(ctx, d, loader)
		hlr, err := newHLR(d)
		if err != nil {
			return err
		}

		return service.ConsumeBatch(ctx, d, "router", []string{kafka.TopicSubmit, kafka.TopicReroute}, 1000, func(ctx context.Context, recs []*kgo.Record) error {
			table := loader.Table()
			out := make([]*kgo.Record, 0, len(recs))
			dealSegs := map[string]int64{} // routed under vendor deals in this batch
			msgs := make([]*domain.Message, len(recs))
			for i, rec := range recs {
				var m domain.Message
				if err := json.Unmarshal(rec.Value, &m); err != nil {
					d.Log.Error("bad submit record; skipping", "offset", rec.Offset, "err", err)
					continue
				}
				msgs[i] = &m
			}
			refused := hlr.apply(ctx, table, msgs)
			reject := func(m *domain.Message, code string) error {
				r, err := kafka.Record(kafka.TopicStatus, m.ID, domain.StatusEvent{
					MessageID: m.ID, AccountID: m.AccountID, HoldID: m.HoldID, Price: m.Price, Charges: m.Charges,
					Status: domain.StatusRejected, ErrorCode: code, At: time.Now().UTC(),
				})
				if err == nil {
					out = append(out, r)
				}
				return err
			}
			for i, mp := range msgs {
				if mp == nil {
					continue
				}
				m := *mp
				if code, ok := refused[i]; ok {
					metrics.Routed.WithLabelValues("none").Inc()
					if err := reject(&m, code); err != nil {
						return err
					}
					continue
				}
				dec, err := table.Choose(m.AccountID, m.MCC, m.MNC, m.Avoid...)
				if errors.Is(err, routing.ErrNoRoute) {
					metrics.Routed.WithLabelValues("none").Inc()
					if err := reject(&m, "NO_ROUTE"); err != nil {
						return err
					}
					continue
				}
				m.Route = &domain.Route{VendorID: dec.VendorID, Cost: dec.Cost}
				if dec.DealID != "" {
					dealSegs[dec.DealID] += int64(m.Segments)
					loader.Progress.Add(dec.DealID, int64(m.Segments))
				}
				metrics.Routed.WithLabelValues(dec.VendorID).Inc()
				r, err := kafka.Record(kafka.RouteTopic(dec.VendorID), m.ID, m)
				if err != nil {
					return err
				}
				out = append(out, r)
			}
			if err := kafka.ProduceJSON(ctx, d.Kafka, out...); err != nil {
				return err
			}
			// Progress only steers routing; the deal report counts from ClickHouse.
			if err := deals.AddRouted(ctx, d.Redis, dealSegs); err != nil {
				d.Log.Warn("vendor deal progress not saved", "err", err)
			}
			return nil
		})
	})
}

// evaluateHealth decides every two seconds which vendors are out of
// routing and publishes the verdicts for connectors and operators.
func evaluateHealth(ctx context.Context, d *service.Deps, loader *routing.Loader) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	prev := map[string]vendorhealth.State{}
	if cur, err := vendorhealth.States(ctx, d.Redis); err == nil {
		prev = cur // keep "since" and cooldowns across router restarts
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		states, err := vendorhealth.Evaluate(ctx, d.Redis, loader.Table().Policies(), prev)
		if err != nil {
			d.Log.Warn("vendor health not evaluated; keeping last verdicts", "err", err)
			continue
		}
		for v, st := range states {
			if p, ok := prev[v]; !ok || p.Up != st.Up || p.Reason != st.Reason {
				d.Log.Warn("vendor routing state changed", "vendor", v, "up", st.Up, "reason", st.Reason, "detail", st.Detail)
			}
			metrics.VendorRoutable.WithLabelValues(v).Set(map[bool]float64{true: 1, false: 0}[st.Up])
		}
		loader.Health.Set(states)
		prev = states
	}
}
