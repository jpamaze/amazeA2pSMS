// Command ch-writer writes message history to ClickHouse: a row when a
// message is accepted (sms.submit) and a row when it reaches its final status
// (sms.records).
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/kafka"
	"github.com/jpamaze/amazeA2pSMS/internal/records"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
	"github.com/jpamaze/amazeA2pSMS/migrations"
)

type chConfig struct {
	URL          string `env:"CLICKHOUSE_URL" envDefault:"http://localhost:8123"`
	Database     string `env:"CLICKHOUSE_DB" envDefault:"sms"`
	User         string `env:"CLICKHOUSE_USER" envDefault:"sms"`
	Password     string `env:"CLICKHOUSE_PASSWORD" envDefault:"sms"`
	EnsureSchema bool   `env:"CLICKHOUSE_ENSURE_SCHEMA" envDefault:"true"`
}

func main() {
	var cc chConfig
	service.Run("ch-writer", service.Needs{Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&cc); err != nil {
			return fmt.Errorf("ch-writer config: %w", err)
		}
		ch := &records.ClickHouse{URL: cc.URL, Database: cc.Database, User: cc.User, Password: cc.Password}
		d.Health.Add("clickhouse", func(ctx context.Context) error { return ch.Exec(ctx, "SELECT 1", nil) })
		if cc.EnsureSchema {
			// ClickHouse may start after us: keep trying instead of exiting.
			for backoff := time.Second; ; backoff = min(backoff*2, 30*time.Second) {
				sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				err := ch.EnsureSchema(sctx, migrations.ClickHouse, "clickhouse")
				cancel()
				if err == nil {
					break
				}
				d.Log.Warn("clickhouse schema not applied yet; retrying", "err", err, "retry_in", backoff)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
			}
		}
		w := &records.Writer{CH: ch}
		return service.ConsumeBatch(ctx, d, "ch-writer", []string{kafka.TopicSubmit, records.Topic}, 10000,
			func(ctx context.Context, recs []*kgo.Record) error { return w.Process(ctx, recs) })
	})
}
