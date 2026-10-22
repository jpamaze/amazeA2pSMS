// Package alerts tells accounts when their available balance (balance +
// credit limit - held) drops below the level they set: once by e-mail, SMS
// and webhook, then again only after it has recovered (blueprint: "e-mail,
// SMS and webhook alerts when available falls below levels set per
// account").
package alerts

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/secrets"
	"github.com/jpamaze/amazeA2pSMS/internal/webhook"
)

// SMTP sends e-mail through a relay. TLS: "starttls" (port 587), "tls"
// (port 465) or "none" (local relays only).
type SMTP struct {
	Host, User, Password, From string
	Port                       int
	TLS                        string
}

// Send delivers one plain-text message.
func (s *SMTP) Send(ctx context.Context, to []string, subject, body string) error {
	if s == nil || s.Host == "" {
		return errors.New("e-mail not configured (SMTP_HOST)")
	}
	addr := net.JoinHostPort(s.Host, fmt.Sprint(s.Port))
	d := net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if s.TLS == "tls" {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: s.Host})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp: %w", err)
	}
	defer c.Close()
	if s.TLS == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if s.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.User, s.Password, s.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(s.From); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", r, err)
		}
	}
	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n",
		headerValue(s.From), headerValue(strings.Join(to, ", ")), mime.QEncoding.Encode("utf-8", headerValue(subject)),
		time.Now().Format(time.RFC1123Z), strings.ReplaceAll(body, "\n", "\r\n"))
	if _, err := wc.Write([]byte(msg)); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	return c.Quit()
}

// headerValue drops control characters, so a value (a reseller's brand
// name in the subject) cannot end the header and inject others or a body.
func headerValue(v string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v)
}

// SMS sends alert texts through the platform's own HTTP API, as an account
// set aside for alerts (its DLT template must match Template).
type SMS struct {
	URL      string // http-api base, e.g. http://http-api:8080
	APIKey   string
	Sender   string
	Template string // {name}, {available}, {threshold}
	Client   *http.Client
}

// Send sends text to each number.
func (s *SMS) Send(ctx context.Context, numbers []string, text string) error {
	if s == nil || s.URL == "" || s.APIKey == "" {
		return errors.New("SMS alerts not configured (ALERT_SMS_URL, ALERT_SMS_API_KEY)")
	}
	var errs []string
	for _, n := range numbers {
		body, _ := json.Marshal(map[string]string{"to": n, "sender": s.Sender, "text": text})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.URL, "/")+"/v1/sms", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", s.APIKey)
		resp, err := client(s.Client).Do(req)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			errs = append(errs, fmt.Sprintf("%s: HTTP %d", n, resp.StatusCode))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func client(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Alerter checks accounts and sends alerts.
type Alerter struct {
	PG      *pgxpool.Pool
	Wallets *wallet.Store
	Secrets *secrets.Box
	Mail    *SMTP
	SMS     *SMS
	HTTP    *http.Client
	Log     *slog.Logger
}

type setting struct {
	account, name, brand string
	threshold            decimal.Decimal
	emails, numbers      []string
	webhook              string
	secret               []byte
	firing               bool
}

// Run checks every interval until ctx ends.
func (a *Alerter) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := a.Check(ctx); err != nil && ctx.Err() == nil {
			a.Log.Warn("alert check failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check runs one pass over every enabled setting.
func (a *Alerter) Check(ctx context.Context) error {
	rows, err := a.PG.Query(ctx, `SELECT s.account_id, x.name, s.low_balance, s.emails, s.sms_numbers, s.webhook_url, s.firing,
		x.webhook_secret_enc, coalesce((WITH RECURSIVE up(id, parent_id, depth) AS (
			SELECT x.id, x.parent_id, 0 UNION ALL
			SELECT p.id, p.parent_id, up.depth + 1 FROM accounts p JOIN up ON p.id = up.parent_id)
		  SELECT b.display_name FROM branding b JOIN up ON up.id = b.account_id ORDER BY up.depth LIMIT 1), '')
		FROM alert_settings s JOIN accounts x ON x.id = s.account_id WHERE s.enabled AND x.status = 'active'`)
	if err != nil {
		return err
	}
	var list []setting
	for rows.Next() {
		var st setting
		var sealed []byte
		if err := rows.Scan(&st.account, &st.name, &st.threshold, &st.emails, &st.numbers, &st.webhook, &st.firing, &sealed, &st.brand); err != nil {
			rows.Close()
			return err
		}
		if len(sealed) > 0 && a.Secrets != nil {
			st.secret, _ = a.Secrets.Open(sealed)
		}
		list = append(list, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, st := range list {
		snap, err := a.Wallets.Get(ctx, st.account)
		if err != nil {
			return err
		}
		avail := snap.Available().Decimal()
		low := avail.LessThan(st.threshold)
		switch {
		case low && !st.firing:
			// Claim the alert so only one alerter instance sends it.
			tag, err := a.PG.Exec(ctx, `UPDATE alert_settings SET firing = true, last_fired_at = now()
				WHERE account_id = $1 AND NOT firing`, st.account)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				a.fire(ctx, st, avail)
			}
		case !low && st.firing:
			tag, err := a.PG.Exec(ctx, `UPDATE alert_settings SET firing = false WHERE account_id = $1 AND firing`, st.account)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				_, _ = a.PG.Exec(ctx, `INSERT INTO alert_events (account_id, kind, available, threshold) VALUES ($1, 'recovered', $2, $3)`,
					st.account, avail, st.threshold)
			}
		}
	}
	return nil
}

// fire sends one low-balance alert on every configured channel and records
// which worked.
func (a *Alerter) fire(ctx context.Context, st setting, avail decimal.Decimal) {
	brand := st.brand
	if brand == "" {
		brand = "SMS platform"
	}
	av, th := avail.StringFixed(2), st.threshold.StringFixed(2)
	delivered, errs := []string{}, []string{}
	note := func(ch string, err error) {
		if err != nil {
			errs = append(errs, ch+": "+err.Error())
			a.Log.Warn("alert channel failed", "account", st.account, "channel", ch, "err", err)
			return
		}
		delivered = append(delivered, ch)
	}
	if len(st.emails) > 0 {
		subject := fmt.Sprintf("[%s] Low balance: %s available", brand, av)
		body := fmt.Sprintf("Account: %s\nAvailable to send: %s (balance + credit limit - messages in flight)\nAlert level: %s\n\n"+
			"Messages are refused once nothing is available. Add funds to keep sending.\n\n- %s", st.name, av, th, brand)
		note("email", a.Mail.Send(ctx, st.emails, subject, body))
	}
	if len(st.numbers) > 0 {
		text := ""
		if a.SMS != nil {
			text = strings.NewReplacer("{name}", st.name, "{available}", av, "{threshold}", th).Replace(a.SMS.Template)
		}
		note("sms", a.SMS.Send(ctx, st.numbers, text))
	}
	if st.webhook != "" {
		note("webhook", a.post(ctx, st, av, th))
	}
	_, err := a.PG.Exec(ctx, `INSERT INTO alert_events (account_id, kind, available, threshold, delivered, errors)
		VALUES ($1, 'low_balance', $2, $3, $4, $5)`, st.account, avail, st.threshold, delivered, strings.Join(errs, "; "))
	if err != nil {
		a.Log.Warn("alert event not recorded", "err", err)
	}
	a.Log.Info("low balance alert sent", "account", st.account, "available", av, "threshold", th, "delivered", delivered)
}

func (a *Alerter) post(ctx context.Context, st setting, av, th string) error {
	body, _ := json.Marshal(map[string]any{"event": "low_balance", "account_id": st.account, "available": av,
		"threshold": th, "at": time.Now().UTC().Format(time.RFC3339)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, st.webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if len(st.secret) > 0 { // same signature as delivery webhooks
		req.Header.Set("X-Amaze-Signature", webhook.Sign(string(st.secret), time.Now(), body))
	}
	resp, err := client(a.HTTP).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
