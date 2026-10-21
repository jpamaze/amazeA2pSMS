// Command admin-api manages accounts, wallets, credentials, credit limits and
// webhooks for the platform and resellers.
package main

import (
	"cmp"
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
	"github.com/jpamaze/amazeA2pSMS/internal/records"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
)

type adminConfig struct {
	Addr       string `env:"ADMIN_ADDR" envDefault:":8081"`
	SecretsKey string `env:"SECRETS_KEY"`
	// Keycloak realm URL, e.g. https://sso.example.com/realms/amaze. Required
	// outside APP_ENV=dev; in dev, leaving it empty enables header sign-in.
	OIDCIssuer string `env:"OIDC_ISSUER"`
	// Client IDs whose tokens are accepted: the admin tools and the portal.
	OIDCAudience string `env:"OIDC_AUDIENCE" envDefault:"amaze-admin,amaze-portal"`
	// Optional: fetch discovery here when it differs from the issuer URL.
	OIDCDiscovery string `env:"OIDC_DISCOVERY_URL"`
	// Optional: usage reports read ClickHouse.
	CHURL      string `env:"CLICKHOUSE_URL"`
	CHDB       string `env:"CLICKHOUSE_DB" envDefault:"sms"`
	CHUser     string `env:"CLICKHOUSE_USER" envDefault:"sms"`
	CHPassword string `env:"CLICKHOUSE_PASSWORD" envDefault:"sms"`
	// Portal: the static export to serve (portal/out), and the public OIDC
	// client the browser signs in with (PKCE). The issuer defaults to
	// OIDC_ISSUER; set it when browsers reach Keycloak at another URL.
	PortalDir      string `env:"PORTAL_DIR"`
	PortalIssuer   string `env:"PORTAL_OIDC_ISSUER"`
	PortalClientID string `env:"PORTAL_OIDC_CLIENT_ID" envDefault:"amaze-portal"`
	// Live monitoring reads Prometheus.
	PromURL string `env:"PROMETHEUS_URL"`
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
		var resolver adminapi.Resolver
		switch {
		case ac.OIDCIssuer != "":
			// Keep trying discovery: Keycloak may start after us.
			for backoff := time.Second; ; backoff = min(backoff*2, 30*time.Second) {
				o, err := adminapi.NewOIDC(ctx, d.PG, ac.OIDCIssuer, ac.OIDCAudience, ac.OIDCDiscovery)
				if err == nil {
					resolver = o
					break
				}
				d.Log.Warn("OIDC issuer not reachable yet; retrying", "err", err, "retry_in", backoff)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
			}
			d.Log.Info("admin sign-in via OIDC", "issuer", ac.OIDCIssuer, "audience", ac.OIDCAudience)
		case d.Cfg.Env == "dev":
			resolver = adminapi.DevHeaders{PG: d.PG}
			d.Log.Warn("admin sign-in via X-Actor headers (dev only; set OIDC_ISSUER to use Keycloak)")
		default:
			return fmt.Errorf("OIDC_ISSUER is required outside APP_ENV=dev")
		}
		wallets := wallet.New(d.Redis)
		api := &adminapi.API{
			PG: d.PG, Wallets: wallets, Secrets: box, Log: d.Log,
			Credits:  credit.New(d.PG, wallets),
			Ledger:   &ledger.Service{PG: d.PG, Wallets: wallets},
			Resolver: resolver,
			Redis:    d.Redis,
			PromURL:  ac.PromURL,
		}
		if ac.OIDCIssuer != "" {
			api.Portal = adminapi.PortalConfig{OIDCIssuer: cmp.Or(ac.PortalIssuer, ac.OIDCIssuer), OIDCClientID: ac.PortalClientID}
		}
		if ac.CHURL != "" {
			api.CH = &records.ClickHouse{URL: ac.CHURL, Database: ac.CHDB, User: ac.CHUser, Password: ac.CHPassword}
		}
		var handler http.Handler = api.Handler()
		if ac.PortalDir != "" {
			handler = adminapi.PortalHandler(ac.PortalDir, handler)
			d.Log.Info("serving the portal", "dir", ac.PortalDir)
		}
		d.Log.Info("admin api listening", "addr", ac.Addr)
		return service.ServeHTTP(ctx, &http.Server{
			Addr:              ac.Addr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		})
	})
}
