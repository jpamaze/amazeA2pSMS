// Command migrate applies the PostgreSQL migrations and exits.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/config"
	applog "github.com/jpamaze/amazeA2pSMS/internal/platform/log"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/pg"
	"github.com/jpamaze/amazeA2pSMS/migrations"
)

func main() {
	var cfg config.Common
	if err := config.Load(&cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log := applog.New("migrate", cfg.LogLevel)
	ctx := context.Background()

	pool, err := pg.New(ctx, cfg.PostgresURL)
	if err != nil {
		log.Error("connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	applied, err := pg.Migrate(ctx, pool, migrations.Postgres, "postgres", log)
	if err != nil {
		log.Error("migrate failed", "err", err)
		os.Exit(1)
	}
	log.Info("migrations done", "applied", len(applied))
}
