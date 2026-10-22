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

	"github.com/jpamaze/amazeA2pSMS/internal/dlt"
	"github.com/jpamaze/amazeA2pSMS/internal/domain"
)

func TestDLTRegistrationsAndCheck(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	h := (&API{PG: pool, Resolver: DevHeaders{PG: pool}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	var plat, cust, other string
	_ = pool.QueryRow(ctx, `INSERT INTO accounts (role, name) VALUES ('platform', 'P') RETURNING id`).Scan(&plat)
	_ = pool.QueryRow(ctx, `INSERT INTO accounts (parent_id, role, name) VALUES ($1, 'customer', 'Bank') RETURNING id`, plat).Scan(&cust)
	_ = pool.QueryRow(ctx, `INSERT INTO accounts (parent_id, role, name) VALUES ($1, 'customer', 'Other') RETURNING id`, plat).Scan(&other)

	call := func(as, method, path, body string) (int, map[string]any) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("X-Actor-Account", as)
		r.Header.Set("X-Actor-User", "u@x")
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
	base := "/v1/accounts/" + cust + "/dlt"

	code, body := call(cust, "POST", base+"/entities", `{"pe_id":"1201160000000000001","name":"Bank"}`)
	expect("customer registers its PE", 200, code, body)
	code, body = call(other, "POST", "/v1/accounts/"+other+"/dlt/entities", `{"pe_id":"1201160000000000001","name":"Thief"}`)
	expect("PE of another account", 409, code, body)

	pe := base + "/entities/1201160000000000001"
	code, body = call(cust, "POST", pe+"/headers/import", "Header,Category\nBANKOT,Transactional\nBANK1,Transactional\n")
	expect("bad header line", 400, code, body)
	if n := len(body["lines"].([]any)); n != 1 {
		t.Fatalf("one bad line: %v", body)
	}
	code, body = call(cust, "POST", pe+"/headers/import", "Header,Category,Status\nBANKOT,Transactional,Active\nBANKSI,Service Implicit,\n567123,Promotional,\n")
	expect("headers", 200, code, body)

	// Portal-style export columns, in any order.
	tpl := "Template ID,Template Name,Template Type,Header,Template Content,Consent Template ID,URLs\n" +
		"1207160000000000001,OTP,Transactional,BANKOT,Your OTP for txn of Rs.{#numeric#} is {#var#}. -Bank,,\n" +
		"1207160000000000002,Loan,Service Implicit,BANKSI;BANKOT,\"Dear {#var#}, your EMI of Rs.{#numeric#} is due. Pay at {#url#}\",,https://bank.in/pay\n" +
		"1207160000000000003,Offer,Promotional,567123,Get a card! Apply at {#url#},,bank.in\n"
	code, body = call(cust, "POST", pe+"/templates/import", tpl)
	expect("templates", 200, code, body)
	code, body = call(cust, "POST", pe+"/templates/import", "Template ID,Template Type,Header,Template Content\n1207160000000000009,Promotional,BANKOT,Hi {#var#}\n")
	expect("promotional template on a transactional header", 400, code, body)
	code, body = call(cust, "POST", pe+"/templates/import", "Template ID,Template Type,Header,Template Content\n1207160000000000008,Service Explicit,BANKSI,Hi {#var#}\n")
	expect("service explicit without consent", 400, code, body)

	check := func(body string) map[string]any {
		t.Helper()
		code, out := call(cust, "POST", base+"/check", body)
		expect("check", 200, code, out)
		return out
	}
	if out := check(`{"sender":"BANKOT","text":"Your OTP for txn of Rs.1,250.00 is 448812. -Bank"}`); out["reason"] != "dlt_tm_chain" {
		t.Fatalf("no chain yet: %v", out)
	}
	code, body = call(cust, "PUT", pe+"/chain", `{"tm_chain":"1201160000000000001,1702000000000000001"}`)
	expect("chain", 200, code, body)
	out := check(`{"sender":"BANKOT","text":"Your OTP for txn of Rs.1,250.00 is 448812. -Bank"}`)
	if out["ok"] != true || out["template_id"] != "1207160000000000001" || out["category"] != "transactional" {
		t.Fatalf("auto-detected OTP: %v", out)
	}
	if out := check(`{"sender":"BANKSI","text":"Dear Asha, your EMI of Rs.5000 is due. Pay at https://bank.in/pay/77"}`); out["ok"] != true {
		t.Fatalf("whitelisted link: %v", out)
	}
	if out := check(`{"sender":"BANKSI","text":"Dear Asha, your EMI of Rs.5000 is due. Pay at https://bank-in.co/pay"}`); out["reason"] != "dlt_url" {
		t.Fatalf("foreign link: %v", out)
	}
	night := time.Date(2026, 10, 21, 22, 30, 0, 0, dlt.IST).Format(time.RFC3339)
	if out := check(`{"sender":"567123","text":"Get a card! Apply at bank.in/c","at":"` + night + `"}`); out["reason"] != "dlt_promo_hours" || out["receipt_code"] != "605" {
		t.Fatalf("promo at night: %v", out)
	}

	// Suspending a template through re-import takes it out of matching.
	code, body = call(cust, "POST", pe+"/templates/import", "Template ID,Template Type,Header,Template Content,Status\n"+
		"1207160000000000001,Transactional,BANKOT,Your OTP for txn of Rs.{#numeric#} is {#var#}. -Bank,Deactivated\n")
	expect("suspend", 200, code, body)
	if out := check(`{"sender":"BANKOT","text":"Your OTP for txn of Rs.1 is 4. -Bank"}`); out["reason"] != "dlt_template" {
		t.Fatalf("suspended template: %v", out)
	}
	code, body = call(cust, "GET", pe+"/templates?header=banksi", "")
	expect("list by header", 200, code, body)

	// Others cannot read or change this account's DLT data; only the platform switches DLT off.
	code, body = call(other, "GET", base, "")
	expect("other account", 403, code, body)
	code, body = call(cust, "PUT", "/v1/accounts/"+cust+"/dlt-mode", `{"mode":"off","reason":"x"}`)
	expect("customer switches DLT off", 403, code, body)
	code, body = call(plat, "PUT", "/v1/accounts/"+cust+"/dlt-mode", `{"mode":"off","reason":"pilot"}`)
	expect("platform switches DLT off", 200, code, body)

	// The checker the edges use sees the same data.
	c := &dlt.Checker{PG: pool, TTL: -1, RequireChain: true}
	if _, err := c.Check(ctx, cust, "NOPE12", "anything", false, domain.DLTFields{}); err != nil {
		t.Fatalf("mode off: %v", err)
	}

	// Vendor receipt codes.
	if _, err := pool.Exec(ctx, `INSERT INTO vendors (id, name, protocol) VALUES ('v1', 'V1', 'smpp')`); err != nil {
		t.Fatal(err)
	}
	code, body = call(plat, "POST", "/v1/vendor-error-codes/import?vendor=v1", "code,reason,customer_code,description\n1703,dlt_template,603,Template mismatch\n1704,dlt_dnd,610,\n")
	expect("error codes", 200, code, body)
	code, body = call(plat, "POST", "/v1/vendor-error-codes/import", "x,Bad Reason,6\n")
	expect("bad error code line", 400, code, body)
	m := &dlt.ErrorMap{PG: pool}
	if err := m.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.Lookup("v1", "1704"); !ok || got.Reason != "dlt_dnd" || got.CustomerCode != "610" {
		t.Fatalf("lookup: %+v %v", got, ok)
	}
	if _, ok := m.Lookup("v2", "1704"); ok {
		t.Fatal("vendor-specific code must not apply to other vendors")
	}
}
