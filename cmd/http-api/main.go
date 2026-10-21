// Command http-api is the customer REST edge: authenticate, rate-limit,
// hold balance, write to Kafka, answer 202. It never calls a vendor
// (blueprint rule 2).
package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/jpamaze/amazeA2pSMS/internal/auth"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/deals"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/hierarchy"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/pricing"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/edge/httpapi"
	"github.com/jpamaze/amazeA2pSMS/internal/ingest"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

type apiConfig struct {
	Addr       string `env:"HTTP_ADDR" envDefault:":8080"`
	AccountTPS int    `env:"HTTP_ACCOUNT_TPS" envDefault:"200"`
}

func main() {
	var ac apiConfig
	service.Run("http-api", service.Needs{Postgres: true, Redis: true, Kafka: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&ac); err != nil {
			return fmt.Errorf("http-api config: %w", err)
		}
		store := msgstore.New(d.Redis)
		wallets := wallet.New(d.Redis)
		api := &httpapi.API{
			Keys: auth.New(d.PG),
			Accept: &ingest.Pipeline{
				Chain:  &hierarchy.Cached{PG: d.PG, TTL: time.Minute},
				Deals:  &deals.Book{PG: d.PG, Redis: d.Redis},
				Pricer: pricing.NewCached(d.PG, time.Minute), Wallets: wallets,
				Store: store, Redis: d.Redis, Kafka: d.Kafka,
			},
			Store: store, Wallets: wallets, Log: d.Log, AccountTPS: ac.AccountTPS,
		}
		d.Log.Info("http api listening", "addr", ac.Addr)
		return service.ServeHTTP(ctx, &http.Server{
			Addr:              ac.Addr,
			Handler:           api.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
		})
	})
}
