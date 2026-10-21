package dlt

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
)

func TestTemplateMatch(t *testing.T) {
	cases := []struct {
		body, text string
		ok         bool
	}{
		{"Your OTP is {#var#}. Do not share it. -Amaze", "Your OTP is 482913. Do not share it. -Amaze", true},
		{"Your OTP is {#var#}. Do not share it. -Amaze", "Your OTP is 482913. Do not share it.", false},
		{"Your OTP is {#var#}. Do not share it. -Amaze", "Your otp is 482913. Do not share it. -Amaze", false},  // literals are case-sensitive
		{"Your OTP is {#var#}. Do not share it. -Amaze", "Your OTP is 482913.  Do not\nshare it. -Amaze", true}, // whitespace runs
		{"Your OTP is {#var#}. Do not share it. -Amaze", "Your OTP is " + strings.Repeat("9", 31) + ". Do not share it. -Amaze", false},
		{"Dear {#var#}, Rs.{#numeric#} credited to A/c {#alphanumeric#}", "Dear Asha Rao, Rs.1,250.00 credited to A/c XX1234", true},
		{"Dear {#var#}, Rs.{#numeric#} credited to A/c {#alphanumeric#}", "Dear Asha Rao, Rs.12 50 credited to A/c XX1234", false},
		{"Track at {#url#} -Amaze", "Track at https://amz.in/t/9 -Amaze", true},
		{"Track at {#url#} -Amaze", "Track at  -Amaze", false}, // url needs a value
		{"{#var#} is your code", "4821 is your code", true},
		{"Hi {#VAR#}", "Hi Ravi", true}, // placeholder names are case-insensitive
		{"आपका OTP {#var#} है", "आपका OTP 1234 है", true},
		{"{#var#}{#var#}", strings.Repeat("a", 60), true},
		{"{#var#}{#var#}", strings.Repeat("a", 61), false},
	}
	for _, c := range cases {
		m, err := Compile(c.body)
		if err != nil {
			t.Fatalf("%q: %v", c.body, err)
		}
		if _, ok := m.Match(c.text); ok != c.ok {
			t.Errorf("%q vs %q: want %v", c.body, c.text, c.ok)
		}
	}
	if _, err := Compile("   "); err == nil {
		t.Fatal("empty body must not compile")
	}
}

func TestWhitelist(t *testing.T) {
	var w Whitelist
	w.Add("url", "https://www.Amz.in/")
	w.Add("url", "pay.example.com/checkout")
	w.Add("apk", "amz.in/app.apk")
	w.Add("callback", "+91 98100 12345")
	ok := []Value{
		{KindVar, "see https://amz.in/t/9?x=1"},
		{KindVar, "visit www.amz.in"},
		{KindVar, "pay.example.com/checkout/42"},
		{KindVar, "Rs.500 for Mr.Rao"}, // not links
		{KindVar, "get amz.in/app.apk"},
		{KindCallback, "09810012345"},
	}
	for _, v := range ok {
		if e := w.checkValues([]Value{v}); e != nil {
			t.Errorf("%+v: %v", v, e)
		}
	}
	bad := map[Reason][]Value{
		ReasonURL: {
			{KindVar, "bit.ly/abc"},
			{KindVar, "https://amz.in.evil.com/x"},
			{KindVar, "pay.example.com/checkoutfake"},
			{KindVar, "evil.com/x.apk"},
			{KindURL, "amz-in.top"}, // typed url values are checked even on an unknown TLD
		},
		ReasonCallback: {{KindCallback, "9810099999"}},
	}
	for want, vals := range bad {
		for _, v := range vals {
			if e := w.checkValues([]Value{v}); e == nil || e.Reason != want {
				t.Errorf("%+v: want %s, got %v", v, want, e)
			}
		}
	}
}

// fixture builds an account in memory: one entity with a transactional and
// a promotional header.
func fixture() *Account {
	ent := &entity{id: "e1", peID: "1101", status: "active", chain: "1101,1702"}
	txn := &header{id: "h1", text: "AMAZEB", category: Transactional, status: "active", ent: ent}
	promo := &header{id: "h2", text: "567890", category: Promotional, status: "active", ent: ent}
	otp := &template{id: "t1", templateID: "1107001", body: "Your OTP is {#var#}. -Amaze", category: Transactional,
		status: "active", ent: ent, headers: map[string]bool{"h1": true}}
	generic := &template{id: "t2", templateID: "1107002", body: "Your {#var#} is {#var#}. -Amaze", category: ServiceImplicit,
		status: "active", ent: ent, headers: map[string]bool{"h1": true}}
	offer := &template{id: "t3", templateID: "1107003", body: "Sale! Shop at {#url#}", category: Promotional,
		status: "active", ent: ent, headers: map[string]bool{"h2": true}}
	offer.wl.Add("url", "amz.in")
	return &Account{
		Mode:     "enforce",
		byPE:     map[string]*entity{"1101": ent},
		byHeader: map[string][]*header{"AMAZEB": {txn}, "567890": {promo}},
		byTplID:  map[string]*template{"1107001": otp, "1107002": generic, "1107003": offer},
		tplsOf:   map[string][]*template{"h1": {otp, generic}, "h2": {offer}},
	}
}

func TestCheck(t *testing.T) {
	noon := time.Date(2026, 10, 21, 12, 0, 0, 0, IST)
	c := &Checker{RequireChain: true, Now: func() time.Time { return noon }}
	a := fixture()

	// Template found automatically; the most specific one wins.
	got, err := c.check(a, "amazeb", "Your OTP is 4821. -Amaze", false, domain.DLTFields{})
	if err != nil || got.TemplateID != "1107001" || got.PEID != "1101" || got.TMChain != "1101,1702" || got.Category != Transactional {
		t.Fatalf("auto-detect: %+v %v", got, err)
	}
	if got, _ := c.check(a, "AMAZEB", "Your order is shipped. -Amaze", false, domain.DLTFields{}); got.TemplateID != "1107002" {
		t.Fatalf("second template: %+v", got)
	}
	// A customer chain wins over the registered one.
	if got, _ := c.check(a, "AMAZEB", "Your OTP is 1. -Amaze", false, domain.DLTFields{TMChain: "x"}); got.TMChain != "x" {
		t.Fatalf("customer chain: %+v", got)
	}

	refusals := []struct {
		sender, text string
		partial      bool
		f            domain.DLTFields
		want         Reason
	}{
		{"AMAZEB", "Your OTP is 1. -Amaze", false, domain.DLTFields{PEID: "9999"}, ReasonEntity},
		{"OTHERS", "Your OTP is 1. -Amaze", false, domain.DLTFields{}, ReasonHeader},
		{"AMAZEB", "Hello there", false, domain.DLTFields{}, ReasonTemplate},
		{"AMAZEB", "Your OTP is 1. -Amaze", false, domain.DLTFields{TemplateID: "1107003"}, ReasonTemplate}, // other header's template
		{"AMAZEB", "Your OTP: 1. -Amaze", false, domain.DLTFields{TemplateID: "1107002"}, ReasonTemplate},   // text does not match it
		{"AMAZEB", "part 1 of a long text", true, domain.DLTFields{}, ReasonTemplate},                       // UDH needs a template ID
		{"567890", "Sale! Shop at bit.ly/x", false, domain.DLTFields{}, ReasonURL},
	}
	for _, r := range refusals {
		_, err := c.check(a, r.sender, r.text, r.partial, r.f)
		e, ok := AsError(err)
		if !ok || e.Reason != r.want {
			t.Errorf("%s %q %+v: want %s, got %v", r.sender, r.text, r.f, r.want, err)
		}
	}
	// UDH part with a template ID: header and template checked, text not.
	if _, err := c.check(a, "AMAZEB", "part 1", true, domain.DLTFields{TemplateID: "1107001"}); err != nil {
		t.Fatalf("UDH with template ID: %v", err)
	}

	// Promotional hours.
	if _, err := c.check(a, "567890", "Sale! Shop at amz.in/d", false, domain.DLTFields{}); err != nil {
		t.Fatalf("promo at noon: %v", err)
	}
	c.Now = func() time.Time { return time.Date(2026, 10, 21, 21, 0, 0, 0, IST) }
	if _, err := c.check(a, "567890", "Sale! Shop at amz.in/d", false, domain.DLTFields{}); err == nil || err.(*Error).Reason != ReasonPromoHours {
		t.Fatalf("promo at 21:00: %v", err)
	}
	// Category against header type.
	a.byTplID["1107003"].headers["h1"] = true
	a.tplsOf["h1"] = append(a.tplsOf["h1"], a.byTplID["1107003"])
	c.Now = func() time.Time { return noon }
	if _, err := c.check(a, "AMAZEB", "Sale! Shop at amz.in/d", false, domain.DLTFields{}); err == nil || err.(*Error).Reason != ReasonCategory {
		t.Fatalf("promo template on transactional header: %v", err)
	}
	// Suspensions and missing chain.
	a.byPE["1101"].chain = ""
	if _, err := c.check(a, "AMAZEB", "Your OTP is 1. -Amaze", false, domain.DLTFields{}); err == nil || err.(*Error).Reason != ReasonChain {
		t.Fatalf("no chain: %v", err)
	}
	a.byHeader["AMAZEB"][0].status = "suspended"
	if _, err := c.check(a, "AMAZEB", "Your OTP is 1. -Amaze", false, domain.DLTFields{TMChain: "x"}); err == nil || err.(*Error).Reason != ReasonHeader {
		t.Fatalf("suspended header: %v", err)
	}
	// Mode off passes anything through.
	a.Mode = "off"
	if got, err := c.check(a, "NOPE", "anything", false, domain.DLTFields{PEID: "1"}); err != nil || got.PEID != "1" {
		t.Fatalf("mode off: %+v %v", got, err)
	}
}

func TestHeaderTypeAndCategory(t *testing.T) {
	if !HeaderTypeOK("AMAZEB", Transactional) || HeaderTypeOK("AMAZ3B", Transactional) || HeaderTypeOK("123456", ServiceImplicit) ||
		!HeaderTypeOK("123456", Promotional) || HeaderTypeOK("AMAZEB", Promotional) || HeaderTypeOK("AMAZE", Transactional) {
		t.Fatal("header type rules")
	}
	for in, want := range map[string]string{"Service Implicit": ServiceImplicit, "SERVICE-EXPLICIT": ServiceExplicit,
		"Transactional": Transactional, "Promotional": Promotional, "SI": ServiceImplicit} {
		if got, ok := NormCategory(in); !ok || got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

// BenchmarkAutoDetect: a header with 500 templates, the message matching
// the last one and no template ID given (the worst case).
func BenchmarkAutoDetect(b *testing.B) {
	ent := &entity{id: "e", peID: "1", status: "active", chain: "c"}
	h := &header{id: "h", text: "AMAZEB", category: Transactional, status: "active", ent: ent}
	a := &Account{Mode: "enforce", byPE: map[string]*entity{"1": ent}, byHeader: map[string][]*header{"AMAZEB": {h}},
		byTplID: map[string]*template{}, tplsOf: map[string][]*template{}}
	for i := range 500 {
		t := &template{id: fmt.Sprint(i), templateID: fmt.Sprint(i), category: Transactional, status: "active", ent: ent,
			headers: map[string]bool{"h": true},
			body:    fmt.Sprintf("Dear {#var#}, your order %d of {#var#} items is shipped and will arrive by {#var#}. -Amaze", i)}
		a.byTplID[t.templateID] = t
		a.tplsOf["h"] = append(a.tplsOf["h"], t)
	}
	c := &Checker{}
	text := "Dear Asha, your order 499 of 3 items is shipped and will arrive by Friday. -Amaze"
	if _, err := c.check(a, "AMAZEB", text, false, domain.DLTFields{}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		_, _ = c.check(a, "AMAZEB", text, false, domain.DLTFields{})
	}
}
