// Command admin-api manages accounts, wallets, credentials, credit limits and
// webhooks for the platform and resellers.
package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/credit"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/ledger"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/edge/adminapi"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/secrets"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

type adminConfig struct {
	Addr       string `env:"ADMIN_ADDR" envDefault:":8081"`
	SecretsKey string `env:"SECRETS_KEY"`
}

func main() {
	var ac adminConfig
	service.Run("admin-api", service.Needs{Postgres: true, Redis: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&ac); err != nil {
			return fmt.Errorf("admin-api config: %w", err)
		}
		box, err := secrets.FromEnv(ac.SecretsKey, d.Cfg.Env)
		if err != nil {
			return err
		}
		wallets := wallet.New(d.Redis)
		api := &adminapi.API{
			PG: d.PG, Wallets: wallets, Secrets: box, Log: d.Log,
			Credits: credit.New(d.PG, wallets),
			Ledger:  &ledger.Service{PG: d.PG, Wallets: wallets},
			DevAuth: d.Cfg.Env == "dev",
		}
		d.Log.Info("admin api listening", "addr", ac.Addr)
		return service.ServeHTTP(ctx, &http.Server{
			Addr:              ac.Addr,
			Handler:           api.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
		})
	})
}
