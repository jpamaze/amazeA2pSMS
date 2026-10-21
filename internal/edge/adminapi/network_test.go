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

	"github.com/redis/go-redis/v9"
)

func TestNetworkAdmin(t *testing.T) {
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
	pool := testDB(t)
	h := (&API{PG: pool, Redis: rdb, Resolver: DevHeaders{PG: pool}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	var plat, cust string
	_ = pool.QueryRow(ctx, `INSERT INTO accounts (role, name) VALUES ('platform', 'P') RETURNING id`).Scan(&plat)
	_ = pool.QueryRow(ctx, `INSERT INTO accounts (parent_id, role, name) VALUES ($1, 'customer', 'C') RETURNING id`, plat).Scan(&cust)
	if _, err := pool.Exec(ctx, `INSERT INTO vendors (id, name, protocol) VALUES ('v1', 'V1', 'smpp')`); err != nil {
		t.Fatal(err)
	}
	call := func(as, method, path, body string) (int, any) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Actor-Account", as)
		r.Header.Set("X-Actor-User", "u@x")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	steps := []struct {
		as, method, path, body string
		want                   int
	}{
		{cust, "POST", "/v1/routes", `{"mcc":"404","mode":"lcr"}`, 403},
		{plat, "POST", "/v1/routes", `{"mcc":"404","mode":"priority"}`, 400},
		{plat, "POST", "/v1/routes", `{"mcc":"404","mode":"lcr","hlr_reject":true}`, 400},
		{plat, "POST", "/v1/routes", `{"mcc":"404","mode":"lcr","hlr":true,"hlr_reject":true}`, 201},
		{plat, "GET", "/v1/routes", ``, 200},
		{plat, "PUT", "/v1/vendors/v1/failover", `{"enabled":true,"max_submit_ms":1500,"max_error_pct":30,"min_dlr_pct":70,"dlr_window_min":10,"cooldown_s":60}`, 200},
		{plat, "PUT", "/v1/vendors/v1/failover", `{"enabled":true,"max_error_pct":130}`, 400},
		{plat, "PUT", "/v1/vendors/nope/failover", `{"enabled":true}`, 404},
		{plat, "GET", "/v1/vendors/health", ``, 200},
		{plat, "POST", "/v1/number-series/import", "prefix,mcc,mnc,operator,circle\n98100,404,45,Airtel,Delhi\n7012,405,861,Jio,Kerala\n", 200},
		{plat, "POST", "/v1/number-series/import", "98,4041,10\n", 400},
		{plat, "GET", "/v1/numbers/9810077777", ``, 200},
		{plat, "GET", "/v1/numbers/12345", ``, 400},
		{plat, "POST", "/v1/firewall/rules", `{"kind":"content","pattern":"re:("}`, 400},
		{plat, "POST", "/v1/firewall/rules", `{"kind":"url","pattern":"evil.in","note":"phishing"}`, 201},
		{plat, "POST", "/v1/firewall/rules", `{"account_id":"` + cust + `","kind":"sender","pattern":"*BANK","action":"flag"}`, 201},
		{cust, "POST", "/v1/firewall/rules", `{"kind":"url","pattern":"x.in"}`, 403},
		{plat, "GET", "/v1/firewall/rules", ``, 200},
		{plat, "DELETE", "/v1/firewall/rules/1", ``, 200},
		{plat, "DELETE", "/v1/firewall/rules/1", ``, 404},
		{plat, "PUT", "/v1/accounts/" + cust + "/firewall-limits", `{"series_per_10min":0}`, 200},
		{cust, "GET", "/v1/accounts/" + cust + "/firewall/blocks", ``, 200},
		{plat, "DELETE", "/v1/accounts/" + cust + "/firewall/blocks/919820000xxx", ``, 404},
	}
	for _, s := range steps {
		if code, out := call(s.as, s.method, s.path, s.body); code != s.want {
			t.Fatalf("%s %s %s: want %d, got %d %v", s.method, s.path, s.body, s.want, code, out)
		}
	}
	_, out := call(plat, "GET", "/v1/numbers/9810077777", "")
	if res := out.(map[string]any)["resolved"].(map[string]any); res["mnc"] != "45" || res["source"] != "series" {
		t.Fatalf("number info: %v", out)
	}
	var dest, series, block int
	_ = pool.QueryRow(ctx, `SELECT dest_per_hour, series_per_10min, series_block_minutes FROM firewall_limits WHERE account_id = $1`, cust).
		Scan(&dest, &series, &block)
	if dest != 50 || series != 0 || block != 60 {
		t.Fatalf("limits: unset fields keep their defaults: %d %d %d", dest, series, block)
	}
}
