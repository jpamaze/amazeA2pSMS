package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/ledger"
	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
)

func TestDealsAndDisputes(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	pool := testDB(t)
	ctx := context.Background()
	wallets := wallet.New(rdb)
	led := &ledger.Service{PG: pool, Wallets: wallets}
	api := &API{PG: pool, Wallets: wallets, Ledger: led, Redis: rdb, Resolver: DevHeaders{PG: pool},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := api.Handler()

	acct := func(parent *string, role, name string) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO accounts (parent_id, role, name) VALUES ($1, $2, $3) RETURNING id`,
			parent, role, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	plat := acct(nil, "platform", "P")
	res := acct(&plat, "reseller", "R")
	sub := acct(&res, "customer", "S")
	direct := acct(&plat, "customer", "C")
	for _, a := range []string{plat, res, sub, direct} {
		t.Cleanup(func() { rdb.Del(context.Background(), "wallet:{"+a+"}", "wallet:{"+a+"}:holds") })
	}
	if _, err := led.TopUp(ctx, ledger.TopUp{AccountID: res, Amount: decimal.NewFromInt(10), IdempotencyKey: "r", Actor: "t"}); err != nil {
		t.Fatal(err)
	}

	call := func(as, method, path, body string) (int, map[string]any) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Actor-Account", as)
		r.Header.Set("X-Actor-User", "u@"+as[:8])
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	expect := func(what string, want, got int, body map[string]any) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: want %d, got %d %v", what, want, got, body)
		}
	}

	// Customer deals: only the account's parent grants them, no overlaps.
	start, end := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339)
	deal := `{"name":"Diwali","mcc":"404","price":"0.09","cap_segments":1000,"starts_at":"` + start + `","ends_at":"` + end + `"}`
	code, body := call(plat, "POST", "/v1/accounts/"+sub+"/deals", deal)
	expect("platform grants a reseller's sub-account", 403, code, body)
	code, body = call(res, "POST", "/v1/accounts/"+sub+"/deals", deal)
	expect("reseller grants its sub-account", 201, code, body)
	dealID := body["id"].(string)
	code, body = call(res, "POST", "/v1/accounts/"+sub+"/deals", deal)
	expect("overlapping deal", 409, code, body)
	code, body = call(res, "POST", "/v1/accounts/"+sub+"/deals", strings.Replace(deal, `"mcc":"404"`, `"mcc":"40"`, 1))
	expect("bad mcc", 400, code, body)
	code, _ = call(sub, "GET", "/v1/accounts/"+sub+"/deals", "")
	expect("customer sees its deals", 200, code, nil)
	code, body = call(plat, "POST", "/v1/deals/"+dealID+"/cancel", "")
	expect("only the granter cancels", 404, code, body)
	code, body = call(res, "POST", "/v1/deals/"+dealID+"/cancel", "")
	expect("granter cancels", 200, code, body)

	// Vendor deals are platform only.
	if _, err := pool.Exec(ctx, `INSERT INTO vendors (id, name, protocol) VALUES ('v1', 'V1', 'smpp')`); err != nil {
		t.Fatal(err)
	}
	vdeal := `{"name":"Q4","mcc":"404","cost":"0.04","commit_segments":1000000,"shortfall_rate":"0.01","starts_at":"` + start + `","ends_at":"` + end + `"}`
	code, body = call(res, "POST", "/v1/vendors/v1/deals", vdeal)
	expect("reseller vendor deal", 403, code, body)
	code, body = call(plat, "POST", "/v1/vendors/v1/deals", vdeal)
	expect("platform vendor deal", 201, code, body)

	// Customer disputes.
	claim := func(as string, amount string) string {
		t.Helper()
		code, body := call(as, "POST", "/v1/accounts/"+as+"/disputes",
			`{"from":"`+start+`","to":"`+end+`","claimed_amount":"`+amount+`","reason":"charged for failed messages","message_ids":["0192f0a0-0000-7000-8000-000000000001"]}`)
		expect("raise dispute", 201, code, body)
		return body["id"].(string)
	}
	code, body = call(res, "POST", "/v1/accounts/"+sub+"/disputes", `{"from":"`+start+`","to":"`+end+`","claimed_amount":"1","reason":"x"}`)
	expect("disputes are raised by the account itself", 403, code, body)

	d1 := claim(sub, "5")
	code, body = call(plat, "POST", "/v1/disputes/"+d1+"/resolve", `{"decision":"approve","amount":"3","note":"ok"}`)
	expect("platform cannot resolve a reseller's sub-account dispute", 404, code, body)
	code, body = call(res, "POST", "/v1/disputes/"+d1+"/resolve", `{"decision":"approve","amount":"6","note":"too much"}`)
	expect("more than claimed", 400, code, body)
	code, body = call(res, "POST", "/v1/disputes/"+d1+"/resolve", `{"decision":"approve","amount":"3","note":"3 of 5 confirmed"}`)
	expect("reseller approves", 200, code, body)
	code, body = call(res, "POST", "/v1/disputes/"+d1+"/resolve", `{"decision":"approve","amount":"3","note":"again"}`)
	expect("second approval", 409, code, body)

	balance := func(a string) (decimal.Decimal, wallet.Money) {
		var b decimal.Decimal
		_ = pool.QueryRow(ctx, `SELECT balance FROM wallets WHERE account_id = $1`, a).Scan(&b)
		snap, _ := wallets.Get(ctx, a)
		return b, snap.Balance
	}
	if db, hot := balance(sub); !db.Equal(decimal.NewFromInt(3)) || hot != wallet.FromDecimal(db) {
		t.Fatalf("sub-account credited: db %s redis %s", db, hot.Decimal())
	}
	if db, hot := balance(res); !db.Equal(decimal.NewFromInt(7)) || hot != wallet.FromDecimal(db) {
		t.Fatalf("reseller paid the credit: db %s redis %s", db, hot.Decimal())
	}
	var kinds string
	_ = pool.QueryRow(ctx, `SELECT string_agg(kind, ',' ORDER BY kind) FROM ledger_entries WHERE reference = 'dispute ' || $1`, d1).Scan(&kinds)
	if kinds != "refund,refund_given" {
		t.Fatalf("ledger kinds: %q", kinds)
	}

	// A reseller that cannot cover the credit leaves the dispute open.
	d2 := claim(sub, "8")
	code, body = call(res, "POST", "/v1/disputes/"+d2+"/resolve", `{"decision":"approve","amount":"8","note":"all"}`)
	expect("reseller short", 422, code, body)
	var st string
	_ = pool.QueryRow(ctx, `SELECT status FROM customer_disputes WHERE id = $1`, d2).Scan(&st)
	if st != "open" {
		t.Fatalf("dispute must be reopened, is %s", st)
	}
	code, body = call(sub, "POST", "/v1/disputes/"+d2+"/withdraw", "")
	expect("claimant withdraws", 200, code, body)

	// The platform resolves direct customers' disputes as a plain refund.
	d3 := claim(direct, "2")
	code, body = call(direct, "GET", "/v1/disputes/"+d3, "")
	expect("claimant reads its dispute", 200, code, body)
	code, body = call(sub, "GET", "/v1/disputes/"+d3, "")
	expect("others cannot", 404, code, body)
	code, body = call(plat, "POST", "/v1/disputes/"+d3+"/resolve", `{"decision":"approve","amount":"2","note":"vendor outage"}`)
	expect("platform approves", 200, code, body)
	if db, _ := balance(direct); !db.Equal(decimal.NewFromInt(2)) {
		t.Fatalf("direct customer refunded: %s", db)
	}
	d4 := claim(direct, "1")
	code, body = call(plat, "POST", "/v1/disputes/"+d4+"/resolve", `{"decision":"reject","note":"delivered per operator"}`)
	expect("platform rejects", 200, code, body)

	// Vendor dispute workflow (reconciliation itself needs ClickHouse).
	var inv, vd string
	if err := pool.QueryRow(ctx, `INSERT INTO vendor_invoices (vendor_id, reference, period_from, period_to, total_segments, total_amount, imported_by)
		VALUES ('v1', 'INV-1', '2026-10-01', '2026-10-31', 100, 5, 't') RETURNING id`).Scan(&inv); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO vendor_disputes (invoice_id, disputed_amount, lines, created_by)
		VALUES ($1, 15, '[]', 't') RETURNING id`, inv).Scan(&vd); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		body string
		want int
	}{
		{`{"status":"settled","settled_amount":"10"}`, 409},
		{`{"status":"sent","note":"emailed to vendor billing"}`, 200},
		{`{"status":"accepted","note":"vendor agrees"}`, 200},
		{`{"status":"settled","settled_amount":"20"}`, 400},
		{`{"status":"settled"}`, 400},
		{`{"status":"settled","settled_amount":"15","note":"credit note CN-9"}`, 200},
		{`{"status":"sent"}`, 409},
	}
	for _, s := range steps {
		code, body := call(plat, "PATCH", "/v1/vendor-disputes/"+vd, s.body)
		expect("vendor dispute "+s.body, s.want, code, body)
	}
	var notes string
	_ = pool.QueryRow(ctx, `SELECT notes FROM vendor_disputes WHERE id = $1`, vd).Scan(&notes)
	if strings.Count(notes, "\n") != 3 {
		t.Fatalf("notes trail: %q", notes)
	}
}

func TestCommitProgress(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 30)
	// 10 days in, 200k of 1M billed: on track for 600k, 400k short.
	p := commitProgress(1_000_000, 200_000, start, end, start.AddDate(0, 0, 10))
	if p["projected_segments"] != int64(600_000) || p["projected_shortfall"] != int64(400_000) || p["needed_per_day"] != int64(40_000) {
		t.Fatalf("mid-deal: %v", p)
	}
	if _, ok := commitProgress(1_000_000, 500, start, end, start.Add(time.Hour))["projected_segments"]; ok {
		t.Fatal("no projection from less than a day")
	}
	// Ended: what was billed is final.
	p = commitProgress(1_000_000, 900_000, start, end, end.AddDate(0, 0, 3))
	if p["projected_shortfall"] != int64(100_000) || p["days_left"] != 0.0 {
		t.Fatalf("ended: %v", p)
	}
}
