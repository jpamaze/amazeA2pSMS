// Command alerter sends low-balance alerts (e-mail over SMTP, SMS through
// the platform, webhooks) when an account's available balance falls below
// the level it set.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/jpamaze/amazeA2pSMS/internal/alerts"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/secrets"
	"github.com/jpamaze/amazeA2pSMS/internal/service"
	"github.com/jpamaze/amazeA2pSMS/internal/webhook"
)

type alerterConfig struct {
	Every        time.Duration `env:"ALERT_EVERY" envDefault:"30s"`
	AllowPrivate bool          `env:"WEBHOOK_ALLOW_PRIVATE"` // dev only: alert webhooks to local test servers
	SecretsKey   string        `env:"SECRETS_KEY"`

	SMTPHost     string `env:"SMTP_HOST"`
	SMTPPort     int    `env:"SMTP_PORT" envDefault:"587"`
	SMTPUser     string `env:"SMTP_USER"`
	SMTPPassword string `env:"SMTP_PASSWORD"`
	SMTPFrom     string `env:"SMTP_FROM"`
	SMTPTLS      string `env:"SMTP_TLS" envDefault:"starttls"` // starttls | tls | none

	// SMS alerts go through the platform's HTTP API as an alerts account
	// whose DLT template matches ALERT_SMS_TEMPLATE.
	SMSURL      string `env:"ALERT_SMS_URL"`
	SMSAPIKey   string `env:"ALERT_SMS_API_KEY"`
	SMSSender   string `env:"ALERT_SMS_SENDER"`
	SMSTemplate string `env:"ALERT_SMS_TEMPLATE" envDefault:"Low balance alert for {name}: {available} available, alert level {threshold}. Please add funds."`
}

func main() {
	var c alerterConfig
	service.Run("alerter", service.Needs{Postgres: true, Redis: true}, func(ctx context.Context, d *service.Deps) error {
		if err := env.Parse(&c); err != nil {
			return fmt.Errorf("alerter config: %w", err)
		}
		box, err := secrets.FromEnv(c.SecretsKey, d.Cfg.Env)
		if err != nil {
			return err
		}
		if c.AllowPrivate && d.Cfg.Env != "dev" {
			return fmt.Errorf("WEBHOOK_ALLOW_PRIVATE is only allowed with APP_ENV=dev")
		}
		// Alert webhooks are customer-chosen URLs: the same guarded client
		// as receipt webhooks (public addresses only, no redirects).
		a := &alerts.Alerter{PG: d.PG, Wallets: wallet.New(d.Redis), Secrets: box, Log: d.Log, HTTP: webhook.NewHTTPClient(c.AllowPrivate),
			Mail: &alerts.SMTP{Host: c.SMTPHost, Port: c.SMTPPort, User: c.SMTPUser, Password: c.SMTPPassword, From: c.SMTPFrom, TLS: c.SMTPTLS},
			SMS:  &alerts.SMS{URL: c.SMSURL, APIKey: c.SMSAPIKey, Sender: c.SMSSender, Template: c.SMSTemplate}}
		if c.SMTPHost == "" {
			d.Log.Warn("SMTP_HOST not set: e-mail alerts will fail and be recorded as such")
		}
		a.Run(ctx, c.Every)
		return nil
	})
}
