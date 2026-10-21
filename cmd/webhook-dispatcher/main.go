// Command webhook-dispatcher POSTs final message status to HTTP customers'
// webhook URLs, signed with HMAC-SHA256, retrying with growing delays.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/twmb/franz-go/pkg/kgo"
	"golang.org/x/sync/errgroup"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/secrets"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
	"github.com/jpamaze/amazeA2pSMS/internal/webhook"
)

type webhookConfig struct {
	SecretsKey string `env:"SECRETS_KEY"`
	Workers    int    `env:"WEBHOOK_WORKERS" envDefault:"128"`
	// Allow webhook URLs on private/loopback addresses (dev only).
	AllowPrivate bool `env:"WEBHOOK_ALLOW_PRIVATE" envDefault:"false"`
}

func main() {
	var wc webhookConfig
	service.Run("webhook-dispatcher", service.Needs{Postgres: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&wc); err != nil {
			return fmt.Errorf("webhook-dispatcher config: %w", err)
		}
		if wc.AllowPrivate && d.Cfg.Env != "dev" {
			return fmt.Errorf("WEBHOOK_ALLOW_PRIVATE is only allowed with APP_ENV=dev")
		}
		box, err := secrets.FromEnv(wc.SecretsKey, d.Cfg.Env)
		if err != nil {
			return err
		}
		disp := &webhook.Dispatcher{
			Targets: &webhook.Targets{PG: d.PG, Box: box, TTL: time.Minute},
			Kafka:   d.Kafka, HTTP: webhook.NewHTTPClient(wc.AllowPrivate), Log: d.Log, Workers: wc.Workers,
		}
		// One consumer per topic: a retry tier waiting out its delay must not
		// hold up first attempts or other tiers.
		topics := []string{webhook.TopicEvents}
		for i := range webhook.RetryDelays {
			topics = append(topics, webhook.RetryTopic(i))
		}
		g, gctx := errgroup.WithContext(ctx)
		for _, t := range topics {
			g.Go(func() error {
				return service.ConsumeBatch(gctx, d, "webhooks-"+t, []string{t}, 1000,
					func(ctx context.Context, recs []*kgo.Record) error { return disp.Process(ctx, recs) })
			})
		}
		return g.Wait()
	})
}
