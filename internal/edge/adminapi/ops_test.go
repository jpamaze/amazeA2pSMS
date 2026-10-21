package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/secrets"
)

func TestOpsEndpoints(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	box, err := secrets.New(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	h := (&API{PG: pool, Secrets: box, Resolver: DevHeaders{PG: pool}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	var plat, cust string
	_ = pool.QueryRow(ctx, `INSERT INTO accounts (role, name) VALUES ('platform', 'P') RETURNING id`).Scan(&plat)
	_ = pool.QueryRow(ctx, `INSERT INTO accounts (parent_id, role, name) VALUES ($1, 'customer', 'C') RETURNING id`, plat).Scan(&cust)
	call := func(as, method, path, body string) (int, any) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Actor-Account", as)
		r.Header.Set("X-Actor-User", "u@x")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: body is not one JSON value: %q", method, path, w.Body.String())
		}
		return w.Code, out
	}
	obj := func(v any) map[string]any { m, _ := v.(map[string]any); return m }
	expect := func(what string, want, got int, body any) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: want %d, got %d %v", what, want, got, body)
		}
	}

	steps := []struct {
		as, method, path, body string
		want                   int
	}{
		{cust, "GET", "/v1/vendors", ``, 403},
		{cust, "POST", "/v1/vendors", `{"id":"acme","name":"Acme","protocol":"smpp"}`, 403},
		{plat, "POST", "/v1/vendors", `{"id":"Acme!","name":"Acme","protocol":"smpp"}`, 400},
		{plat, "POST", "/v1/vendors", `{"id":"acme","name":"Acme","protocol":"smtp"}`, 400},
		{plat, "POST", "/v1/vendors", `{"id":"acme","name":"Acme","protocol":"smpp"}`, 201},
		{plat, "POST", "/v1/vendors", `{"id":"acme","name":"Acme again","protocol":"smpp"}`, 409},
		{plat, "POST", "/v1/vendors", `{"id":"webv","name":"Web vendor","protocol":"http"}`, 201},
		{plat, "PUT", "/v1/vendors/acme/status", `{"status":"gone"}`, 400},
		{plat, "PUT", "/v1/vendors/nope/status", `{"status":"active"}`, 404},
		{plat, "POST", "/v1/vendors/webv/binds", `{"host":"h","port":2775,"system_id":"s","password":"p"}`, 400},
		{plat, "POST", "/v1/vendors/acme/binds", `{"host":"h","port":0,"system_id":"s","password":"p"}`, 400},
		{plat, "POST", "/v1/vendors/acme/binds", `{"host":"h","port":2775,"system_id":"s","password":"toolongpass"}`, 400},
		{plat, "POST", "/v1/vendors/acme/binds", `{"host":"h","port":2775,"system_id":"s","password":"p","smpp_version":"4.0"}`, 400},
		{plat, "POST", "/v1/vendors/acme/binds", `{"host":"h","port":2775,"system_id":"s","password":"p","tlv_profile":{"pe_id":"zz"}}`, 400},
		{plat, "POST", "/v1/vendors/acme/binds", `{"host":"h","port":2775,"system_id":"s","password":"p","tlv_profile":{"colour":"0x1400"}}`, 400},
		{plat, "PUT", "/v1/vendors/acme/http-profile", `{"base_url":"https://x.in"}`, 400},
		{plat, "PUT", "/v1/vendors/webv/http-profile", `{"base_url":"ftp://x.in","request_template":{},"response_mapping":{"id_path":"id"},"dlr_mapping":{"id_param":"id","status_param":"s"}}`, 400},
		{plat, "PUT", "/v1/vendors/webv/http-profile", `{"base_url":"https://x.in","request_template":{},"response_mapping":{},"dlr_mapping":{"id_param":"id","status_param":"s"}}`, 400},
		{plat, "PUT", "/v1/vendors/webv/http-profile", `{"base_url":"https://x.in","request_template":{},"response_mapping":{"id_path":"id"}}`, 400},
	}
	for _, s := range steps {
		code, body := call(s.as, s.method, s.path, s.body)
		expect(s.method+" "+s.path+" "+s.body, s.want, code, body)
	}

	// SMPP bind: password sealed, never returned; disabled once.
	code, body := call(plat, "POST", "/v1/vendors/acme/binds", `{"host":"smsc.acme.in","port":2775,"system_id":"amz","password":"s3cret","tlv_profile":{"pe_id":"0x1400","template_id":"0x1401"}}`)
	expect("add bind", 201, code, body)
	bind := obj(body)["id"].(string)
	var sealed []byte
	_ = pool.QueryRow(ctx, `SELECT password_enc FROM vendor_binds WHERE id = $1`, bind).Scan(&sealed)
	if plain, err := box.Open(sealed); err != nil || string(plain) != "s3cret" || strings.Contains(string(sealed), "s3cret") {
		t.Fatalf("password must be sealed: %v %q", err, plain)
	}
	_, body = call(plat, "GET", "/v1/vendors/acme", "")
	binds := obj(body)["binds"].([]any)
	if len(binds) != 1 || obj(binds[0])["bind_type"] != "transceiver" || obj(binds[0])["window_size"] != 10.0 || strings.Contains(mustJSON(body), "s3cret") {
		t.Fatalf("vendor detail: %v", body)
	}
	_, body = call(plat, "GET", "/v1/vendors", "")
	if v := obj(body.([]any)[0]); v["id"] != "acme" || v["status"] != "paused" || v["binds"] != 1.0 {
		t.Fatalf("vendors: %v", body)
	}
	code, body = call(plat, "DELETE", "/v1/vendors/acme/binds/"+bind, "")
	expect("disable bind", 200, code, body)
	code, body = call(plat, "DELETE", "/v1/vendors/acme/binds/"+bind, "")
	expect("disable bind again", 404, code, body)
	code, body = call(plat, "PUT", "/v1/vendors/acme/status", `{"status":"active"}`)
	expect("activate", 200, code, body)

	// HTTP profile: the secret is kept when a later save leaves it out.
	profile := `"base_url":"https://api.web.in","auth":{"type":"bearer"},"request_template":{"path":"/send","params":{"to":"{{to}}"}},"response_mapping":{"id_path":"id"},"dlr_mapping":{"id_param":"mid","status_param":"st","delivered_values":["DELIVRD"]}`
	code, body = call(plat, "PUT", "/v1/vendors/webv/http-profile", `{`+profile+`,"secret":"tok"}`)
	expect("save profile", 200, code, body)
	code, body = call(plat, "PUT", "/v1/vendors/webv/http-profile", `{`+profile+`,"tps_limit":40}`)
	expect("save profile again", 200, code, body)
	_, body = call(plat, "GET", "/v1/vendors/webv", "")
	p := obj(obj(body)["http_profile"])
	if p["has_secret"] != true || p["tps_limit"] != 40.0 || !strings.HasPrefix(p["dlr_path"].(string), "/dlr/webv?token=") {
		t.Fatalf("profile: %v", body)
	}

	// Costs: current, scheduled and superseded rows.
	past, future := time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339), time.Now().Add(48*time.Hour).UTC().Format(time.RFC3339)
	code, body = call(plat, "POST", "/v1/vendors/acme/costs/import", "mcc,mnc,category,cost,valid_from\n404,*,*,0.10,"+past+"\n404,*,*,0.11,"+time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+"\n404,*,*,0.12,"+future+"\n")
	expect("import costs", 200, code, body)
	_, body = call(plat, "GET", "/v1/vendors/acme/costs", "")
	var states []string
	for _, c := range body.([]any) {
		states = append(states, obj(c)["state"].(string)+":"+obj(c)["cost"].(string))
	}
	if strings.Join(states, ",") != "scheduled:0.1200,current:0.1100,superseded:0.1000" {
		t.Fatalf("costs: %v", states)
	}

	// Vendor invoice without ClickHouse: stored, one clean response.
	day := time.Now().Format(time.DateOnly)
	code, body = call(plat, "POST", "/v1/vendors/acme/invoices?reference=INV-1&from="+day+"&to="+day, day+",404,10,100,10.00\n")
	expect("import invoice", 201, code, body)
	if obj(body)["reconciliation"] != "unavailable" {
		t.Fatalf("invoice: %v", body)
	}
	_, body = call(plat, "GET", "/v1/vendor-invoices?vendor=acme", "")
	if inv := body.([]any); len(inv) != 1 || obj(inv[0])["reference"] != "INV-1" || obj(inv[0])["total_segments"] != 100.0 {
		t.Fatalf("invoices: %v", body)
	}

	// Firewall limits: defaults until set; the account may read its own.
	_, body = call(cust, "GET", "/v1/accounts/"+cust+"/firewall-limits", "")
	if obj(body)["custom"] != false || obj(body)["dest_per_hour"] != 50.0 {
		t.Fatalf("default limits: %v", body)
	}
	code, body = call(plat, "PUT", "/v1/accounts/"+cust+"/firewall-limits", `{"dest_per_hour":20}`)
	expect("set limits", 200, code, body)
	_, body = call(cust, "GET", "/v1/accounts/"+cust+"/firewall-limits", "")
	if obj(body)["custom"] != true || obj(body)["dest_per_hour"] != 20.0 {
		t.Fatalf("custom limits: %v", body)
	}
	code, _ = call(cust, "GET", "/v1/accounts/"+plat+"/firewall-limits", "")
	expect("another account's limits", 403, code, nil)

	// DLT headers with their active template count.
	pe := "1101234567890123499"
	code, body = call(cust, "POST", "/v1/accounts/"+cust+"/dlt/entities", `{"pe_id":"`+pe+`","name":"C Ltd"}`)
	expect("entity", 200, code, body)
	code, body = call(cust, "POST", "/v1/accounts/"+cust+"/dlt/entities/"+pe+"/headers/import", "header,category\nCLTDOT,transactional\n567890,promotional\n")
	expect("headers", 200, code, body)
	code, body = call(cust, "POST", "/v1/accounts/"+cust+"/dlt/entities/"+pe+"/templates/import", "template_id,category,header,body\n1107000000000000001,transactional,CLTDOT,Your code is {#var#}\n")
	expect("templates", 200, code, body)
	_, body = call(cust, "GET", "/v1/accounts/"+cust+"/dlt/entities/"+pe+"/headers", "")
	if hs := body.([]any); len(hs) != 2 || obj(hs[1])["header"] != "CLTDOT" || obj(hs[1])["active_templates"] != 1.0 || obj(hs[0])["active_templates"] != 0.0 {
		t.Fatalf("headers: %v", body)
	}

	// Number data lists.
	code, body = call(plat, "POST", "/v1/number-series/import", "98100,404,45,Airtel,Delhi\n7012,405,861,Jio,Kerala\n")
	expect("series", 200, code, body)
	_, body = call(plat, "GET", "/v1/number-series?q=jio", "")
	if obj(body)["total"] != 2.0 || len(obj(body)["series"].([]any)) != 1 {
		t.Fatalf("series: %v", body)
	}
	code, body = call(plat, "GET", "/v1/mnp/imports", "")
	expect("mnp imports", 200, code, body)
	code, _ = call(cust, "GET", "/v1/number-series", "")
	expect("series are platform only", 403, code, nil)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
