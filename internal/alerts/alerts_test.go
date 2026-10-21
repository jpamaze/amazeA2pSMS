package alerts

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
	"github.com/jpamaze/amazeA2pSMS/internal/platform/pgtest"
)

// fakeSMTP accepts one connection at a time and records each message.
type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	mail []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { io.WriteString(c, s+"\r\n") }
	say("220 fake ESMTP")
	var data strings.Builder
	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if inData {
			if line == ".\r\n" {
				inData = false
				f.mu.Lock()
				f.mail = append(f.mail, data.String())
				f.mu.Unlock()
				data.Reset()
				say("250 queued")
				continue
			}
			data.WriteString(line)
			continue
		}
		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250 fake")
		case cmd == "DATA":
			inData = true
			say("354 go ahead")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func TestLowBalanceAlerts(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	pool := pgtest.Pool(t)
	var plat, res, cust string
	_ = pool.QueryRow(ctx, `INSERT INTO accounts (role, name) VALUES ('platform', 'P') RETURNING id`).Scan(&plat)
	_ = pool.QueryRow(ctx, `INSERT INTO accounts (parent_id, role, name) VALUES ($1, 'reseller', 'R') RETURNING id`, plat).Scan(&res)
	_ = pool.QueryRow(ctx, `INSERT INTO accounts (parent_id, role, name) VALUES ($1, 'customer', 'Shop') RETURNING id`, res).Scan(&cust)
	if _, err := pool.Exec(ctx, `INSERT INTO branding (account_id, display_name, updated_by) VALUES ($1, 'BlueText', 't')`, res); err != nil {
		t.Fatal(err)
	}
	wallets := wallet.New(rdb)
	t.Cleanup(func() { rdb.Del(ctx, "wallet:{"+cust+"}", "wallet:{"+cust+"}:holds") })
	_ = wallets.Sync(ctx, cust, wallet.FromDecimal(decimal.NewFromInt(100)), 0)

	smtpd := newFakeSMTP(t)
	host, port, _ := net.SplitHostPort(smtpd.ln.Addr().String())
	var smsMu sync.Mutex
	var sms []map[string]string
	smsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		_ = json.NewDecoder(r.Body).Decode(&m)
		smsMu.Lock()
		sms = append(sms, m)
		smsMu.Unlock()
		if r.Header.Get("X-API-Key") != "alerts-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(smsSrv.Close)
	hookHits := 0
	hook := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hookHits++
		w.WriteHeader(http.StatusInternalServerError) // a failing channel is recorded, others still go out
	}))
	t.Cleanup(hook.Close)

	if _, err := pool.Exec(ctx, `INSERT INTO alert_settings (account_id, low_balance, emails, sms_numbers, webhook_url, updated_by)
		VALUES ($1, 50, '{ops@shop.in}', '{919810012345}', $2, 't')`, cust, hook.URL); err != nil {
		t.Fatal(err)
	}
	portN, _ := strconv.Atoi(port)
	a := &Alerter{PG: pool, Wallets: wallets, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), HTTP: hook.Client(),
		Mail: &SMTP{Host: host, Port: portN, From: "alerts@platform.in", TLS: "none"},
		SMS:  &SMS{URL: smsSrv.URL, APIKey: "alerts-key", Sender: "ALERTS", Template: "Low balance for {name}: {available} left"}}

	check := func() {
		t.Helper()
		if err := a.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	check() // 100 available, level 50: nothing
	if len(smtpd.mail) != 0 || len(sms) != 0 {
		t.Fatal("no alert above the level")
	}
	_ = wallets.Sync(ctx, cust, wallet.FromDecimal(decimal.NewFromInt(30)), 0)
	check()
	check() // still low: not sent again
	smtpd.mu.Lock()
	mails := append([]string(nil), smtpd.mail...)
	smtpd.mu.Unlock()
	if len(mails) != 1 || !strings.Contains(mails[0], "Subject: [BlueText] Low balance: 30.00 available") ||
		!strings.Contains(mails[0], "Account: Shop") {
		t.Fatalf("e-mail (with the reseller's brand): %q", mails)
	}
	if len(sms) != 1 || sms[0]["to"] != "919810012345" || sms[0]["text"] != "Low balance for Shop: 30.00 left" || sms[0]["sender"] != "ALERTS" {
		t.Fatalf("sms: %+v", sms)
	}
	if hookHits != 1 {
		t.Fatalf("webhook calls: %d", hookHits)
	}
	var delivered []string
	var errs string
	if err := pool.QueryRow(ctx, `SELECT delivered, errors FROM alert_events WHERE account_id = $1 AND kind = 'low_balance'`, cust).
		Scan(&delivered, &errs); err != nil {
		t.Fatal(err)
	}
	if strings.Join(delivered, ",") != "email,sms" || !strings.Contains(errs, "webhook: HTTP 500") {
		t.Fatalf("event: %v %q", delivered, errs)
	}

	// Top-up: re-armed; the next drop alerts again.
	_ = wallets.Sync(ctx, cust, wallet.FromDecimal(decimal.NewFromInt(80)), 0)
	check()
	var firing bool
	_ = pool.QueryRow(ctx, `SELECT firing FROM alert_settings WHERE account_id = $1`, cust).Scan(&firing)
	if firing {
		t.Fatal("recovered: re-armed")
	}
	_ = wallets.Sync(ctx, cust, wallet.FromDecimal(decimal.NewFromInt(10)), 0)
	check()
	if len(sms) != 2 {
		t.Fatalf("second drop alerts again: %d", len(sms))
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM alert_events WHERE account_id = $1`, cust).Scan(&n)
	if n != 3 {
		t.Fatalf("events (low, recovered, low): %d", n)
	}
}
