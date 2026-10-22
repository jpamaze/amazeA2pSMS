package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/jpamaze/amazeA2pSMS/internal/billing/wallet"
)

func TestPortalEndpoints(t *testing.T) {
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
	api := &API{PG: pool, Redis: rdb, Wallets: wallet.New(rdb), Resolver: DevHeaders{PG: pool}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := api.Handler()

	mk := func(parent *string, role, name string) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO accounts (parent_id, role, name) VALUES ($1, $2, $3) RETURNING id`, parent, role, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	plat := mk(nil, "platform", "Amaze")
	res := mk(&plat, "reseller", "BlueText")
	shop := mk(&res, "customer", "Shop")
	direct := mk(&plat, "customer", "Direct")
	for _, u := range []struct{ acct, email, role string }{{plat, "ops@x", "owner"}, {res, "r@x", "owner"}, {shop, "s@x", "owner"}, {shop, "s2@x", "viewer"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO users (account_id, email, role) VALUES ($1, $2, $3)`, u.acct, u.email, u.role); err != nil {
			t.Fatal(err)
		}
	}
	call := func(as, user, method, path, body string, host ...string) (int, any) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if as != "" {
			r.Header.Set("X-Actor-Account", as)
			r.Header.Set("X-Actor-User", user)
		}
		if len(host) > 0 {
			r.Host = host[0]
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	expect := func(what string, want, got int, body any) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: want %d, got %d %v", what, want, got, body)
		}
	}
	obj := func(v any) map[string]any { m, _ := v.(map[string]any); return m }

	// Public config: dev sign-in, platform default brand.
	code, body := call("", "", "GET", "/v1/portal/config", "")
	expect("config", 200, code, body)
	if obj(body)["dev_sign_in"] != true || obj(obj(body)["brand"])["name"] != DefaultBrand.Name {
		t.Fatalf("config: %v", body)
	}
	code, body = call("", "", "GET", "/v1/portal/dev-users", "")
	if code != 200 || len(body.([]any)) != 4 {
		t.Fatalf("dev users: %d %v", code, body)
	}

	// Branding: resellers brand themselves; sub-accounts inherit; a domain finds it.
	code, body = call(shop, "s@x", "PUT", "/v1/accounts/"+shop+"/branding", `{"name":"X"}`)
	expect("customer has no brand", 400, code, body)
	code, body = call(res, "r@x", "PUT", "/v1/accounts/"+res+"/branding", `{"name":"BlueText SMS","primary_color":"#0e7c66","domain":"SMS.BlueText.example","logo":"javascript:alert(1)"}`)
	expect("bad logo", 400, code, body)
	code, body = call(res, "r@x", "PUT", "/v1/accounts/"+res+"/branding", `{"name":"BlueText SMS","primary_color":"#0e7c66","domain":"SMS.BlueText.example","support_email":"help@bluetext.example"}`)
	expect("a reseller cannot claim a domain", 403, code, body)
	code, body = call(res, "r@x", "PUT", "/v1/accounts/"+res+"/branding", "{\"name\":\"X\r\nBcc: victim@x.in\"}")
	expect("no line breaks in a brand name (it goes into e-mail subjects)", 400, code, body)
	code, body = call(plat, "ops@x", "PUT", "/v1/accounts/"+res+"/branding", `{"name":"BlueText SMS","primary_color":"#0e7c66","domain":"SMS.BlueText.example","support_email":"help@bluetext.example"}`)
	expect("the platform sets the reseller's domain", 200, code, body)
	code, body = call(res, "r@x", "PUT", "/v1/accounts/"+res+"/branding", `{"name":"BlueText SMS","primary_color":"#0e7c66","domain":"sms.bluetext.example","support_email":"help@bluetext.example"}`)
	expect("reseller edits its brand, domain unchanged", 200, code, body)
	code, body = call(direct, "d@x", "PUT", "/v1/accounts/"+plat+"/branding", `{"name":"Z","domain":"sms.bluetext.example"}`)
	expect("other account's brand", 403, code, body)
	code, body = call(plat, "ops@x", "PUT", "/v1/accounts/"+plat+"/branding", `{"name":"Amaze","domain":"sms.bluetext.example"}`)
	expect("domain taken", 409, code, body)
	code, body = call("", "", "GET", "/v1/portal/config", "", "sms.bluetext.example:443")
	if code != 200 || obj(obj(body)["brand"])["name"] != "BlueText SMS" {
		t.Fatalf("brand by host: %v", body)
	}
	_, body = call(shop, "s@x", "GET", "/v1/me", "")
	if b := obj(obj(body)["brand"]); b["name"] != "BlueText SMS" || b["primary_color"] != "#0e7c66" || obj(body)["account_name"] != "Shop" {
		t.Fatalf("sub-account inherits the reseller's brand: %v", body)
	}
	_, body = call(direct, "d@x", "GET", "/v1/me", "")
	if obj(obj(body)["brand"])["name"] != DefaultBrand.Name {
		t.Fatalf("direct customer gets the platform brand: %v", body)
	}

	// Accounts list is scoped by role.
	names := func(v any) string {
		var n []string
		for _, a := range v.([]any) {
			n = append(n, obj(a)["name"].(string))
		}
		return strings.Join(n, ",")
	}
	_, body = call(plat, "ops@x", "GET", "/v1/accounts", "")
	if names(body) != "Direct,Shop,Amaze,BlueText" {
		t.Fatalf("platform sees all: %s", names(body))
	}
	_, body = call(res, "r@x", "GET", "/v1/accounts?q=sho", "")
	if names(body) != "Shop" {
		t.Fatalf("reseller sees its sub-accounts: %s", names(body))
	}
	_, body = call(shop, "s@x", "GET", "/v1/accounts", "")
	if names(body) != "Shop" {
		t.Fatalf("customer sees itself: %s", names(body))
	}
	code, _ = call(res, "r@x", "GET", "/v1/accounts/"+direct, "")
	expect("reseller on a foreign account", 403, code, nil)
	code, _ = call(shop, "s@x", "GET", "/v1/accounts/"+shop+"/wallet", "")
	expect("own wallet", 200, code, nil)

	// Status: managers only, never yourself.
	code, body = call(res, "r@x", "PUT", "/v1/accounts/"+shop+"/status", `{"status":"suspended"}`)
	expect("suspend sub-account", 200, code, body)
	code, _ = call(shop, "s@x", "GET", "/v1/me", "")
	expect("suspended account cannot sign in", 401, code, nil)
	code, body = call(res, "r@x", "PUT", "/v1/accounts/"+shop+"/status", `{"status":"active"}`)
	expect("reactivate", 200, code, body)
	code, body = call(res, "r@x", "PUT", "/v1/accounts/"+res+"/status", `{"status":"suspended"}`)
	expect("suspend yourself", 403, code, body)
	code, body = call(plat, "ops@x", "PUT", "/v1/accounts/"+shop+"/status", `{"status":"suspended"}`)
	expect("platform suspends a reseller's sub-account", 200, code, body)
	code, body = call(res, "r@x", "PUT", "/v1/accounts/"+shop+"/status", `{"status":"active"}`)
	expect("the reseller cannot lift a platform suspension", 403, code, body)
	code, body = call(plat, "ops@x", "PUT", "/v1/accounts/"+shop+"/status", `{"status":"active"}`)
	expect("the platform can", 200, code, body)

	// API keys: self-service create, list without secrets, revoke once.
	code, body = call(shop, "s@x", "POST", "/v1/accounts/"+shop+"/api-keys", `{"name":"prod"}`)
	expect("create key", 201, code, body)
	_, body = call(shop, "s@x", "GET", "/v1/accounts/"+shop+"/api-keys", "")
	keys := body.([]any)
	if len(keys) != 1 || obj(keys[0])["key_hash"] != nil {
		t.Fatalf("keys: %v", keys)
	}
	kid := obj(keys[0])["id"].(string)
	code, _ = call(shop, "s@x", "DELETE", "/v1/accounts/"+shop+"/api-keys/"+kid, "")
	expect("revoke", 200, code, nil)
	code, _ = call(shop, "s@x", "DELETE", "/v1/accounts/"+shop+"/api-keys/"+kid, "")
	expect("revoke again", 404, code, nil)

	// Users: list; you cannot remove yourself.
	_, body = call(shop, "s@x", "GET", "/v1/accounts/"+shop+"/users", "")
	var selfID, otherID string
	for _, u := range body.([]any) {
		if obj(u)["email"] == "s@x" {
			selfID = obj(u)["id"].(string)
		} else {
			otherID = obj(u)["id"].(string)
		}
	}
	code, _ = call(shop, "s@x", "DELETE", "/v1/accounts/"+shop+"/users/"+selfID, "")
	expect("remove yourself", 400, code, nil)
	code, _ = call(shop, "s@x", "DELETE", "/v1/accounts/"+shop+"/users/"+otherID, "")
	expect("remove viewer", 200, code, nil)

	// Alerts: validation and round trip (numbers normalised).
	code, body = call(shop, "s@x", "PUT", "/v1/accounts/"+shop+"/alerts", `{"low_balance":"100","emails":["nope"],"enabled":true}`)
	expect("bad e-mail", 400, code, body)
	code, body = call(shop, "s@x", "PUT", "/v1/accounts/"+shop+"/alerts", `{"low_balance":"100","webhook_url":"http://x.in","enabled":true}`)
	expect("plain http webhook", 400, code, body)
	code, body = call(shop, "s@x", "PUT", "/v1/accounts/"+shop+"/alerts", `{"low_balance":"100","emails":["a@shop.in"],"sms_numbers":["98100 12345"],"enabled":true}`)
	expect("save alerts", 200, code, body)
	_, body = call(shop, "s@x", "GET", "/v1/accounts/"+shop+"/alerts", "")
	if s := obj(obj(body)["settings"]); s["low_balance"] != "100" || s["sms_numbers"].([]any)[0] != "919810012345" {
		t.Fatalf("alerts: %v", body)
	}
	code, _ = call(res, "r@x", "GET", "/v1/monitor/live", "")
	expect("live monitor is platform only", 403, code, nil)
}

func TestPortalHandler(t *testing.T) {
	dir := t.TempDir()
	for f, c := range map[string]string{"index.html": "home", "messages/index.html": "messages", "404.html": "nf", "_next/static/a.js": "js"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755)
		_ = os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644)
	}
	apiHit := false
	h := PortalHandler(dir, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { apiHit = true }))
	get := func(p string) (int, string, http.Header) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		return w.Code, w.Body.String(), w.Header()
	}
	if c, b, _ := get("/messages/"); c != 200 || b != "messages" {
		t.Fatalf("page: %d %q", c, b)
	}
	if c, b, _ := get("/no/such/page/"); c != 200 || b != "home" {
		t.Fatalf("unknown route falls back to the app: %d %q", c, b)
	}
	if _, _, hd := get("/_next/static/a.js"); !strings.Contains(hd.Get("Cache-Control"), "immutable") {
		t.Fatal("hashed assets are cached")
	}
	if c, b, _ := get("/../../etc/passwd"); c != 200 || b != "home" {
		t.Fatalf("no escape from the portal directory: %d %q", c, b)
	}
	if _, _, hd := get("/"); hd.Get("X-Frame-Options") != "DENY" {
		t.Fatal("pages must not be framed")
	}
	get("/v1/me")
	if !apiHit {
		t.Fatal("/v1 goes to the API")
	}
}

func TestSecureHeaders(t *testing.T) {
	h := SecureHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "https://sso.example.com/realms/amaze")
	get := func(p string, hdr ...string) http.Header {
		r := httptest.NewRequest("GET", p, nil)
		if len(hdr) == 2 {
			r.Header.Set(hdr[0], hdr[1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Header()
	}
	api := get("/v1/accounts/x/statement")
	if api.Get("Cache-Control") != "no-store" || api.Get("X-Content-Type-Options") != "nosniff" || api.Get("Strict-Transport-Security") != "" {
		t.Fatalf("api headers: %v", api)
	}
	page := get("/messages/", "X-Forwarded-Proto", "https")
	csp := page.Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self' https://sso.example.com;") || !strings.Contains(csp, "frame-ancestors 'none'") ||
		page.Get("Strict-Transport-Security") == "" {
		t.Fatalf("page headers: %v", page)
	}
}

func TestCSVText(t *testing.T) {
	for in, want := range map[string]string{"=HYPERLINK(\"x\")": "'=HYPERLINK(\"x\")", "+1": "'+1", "@SUM(A1)": "'@SUM(A1)", "-2+3": "'-2+3", "UTR 123": "UTR 123", "": ""} {
		if got := csvText(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}
