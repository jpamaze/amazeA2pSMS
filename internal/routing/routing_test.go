package routing

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/jpamaze/amazeA2pSMS/internal/vendorhealth"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestChoose(t *testing.T) {
	active := map[string]bool{"cheap": true, "dear": true, "down": false, "prio": true}
	costs := map[string]map[string]decimal.Decimal{
		"cheap": {"404|*": d("0.05")},
		"dear":  {"404|*": d("0.09"), "404|45": d("0.03")},
		"down":  {"404|*": d("0.01")},
	}
	rules := []Rule{
		{MCC: "404", MNC: "*", Mode: "lcr", Priority: 100},
		{AccountID: "vip", MCC: "404", MNC: "*", Mode: "priority", VendorID: "down", Priority: 1},
		{AccountID: "vip", MCC: "404", MNC: "*", Mode: "priority", VendorID: "prio", Priority: 2},
	}
	tbl := NewTable(rules, active, costs)

	got, err := tbl.Choose("anyone", "404", "*")
	if err != nil || got.VendorID != "cheap" || !got.Cost.Equal(d("0.05")) {
		t.Fatalf("lcr skips inactive cheaper vendor: %+v %v", got, err)
	}
	got, _ = tbl.Choose("anyone", "404", "45")
	if got.VendorID != "dear" {
		t.Fatalf("lcr uses network-specific cost: %+v", got)
	}
	got, _ = tbl.Choose("vip", "404", "*")
	if got.VendorID != "prio" {
		t.Fatalf("account rule falls over to next priority when vendor is down: %+v", got)
	}
	if _, err := tbl.Choose("anyone", "310", "*"); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("unknown country: %v", err)
	}
}

func TestWeighted(t *testing.T) {
	tbl := NewTable([]Rule{
		{MCC: "404", MNC: "*", Mode: "weighted", VendorID: "a", Priority: 1, Weight: 3},
		{MCC: "404", MNC: "*", Mode: "weighted", VendorID: "b", Priority: 1, Weight: 1},
	}, map[string]bool{"a": true, "b": true}, nil)
	n := map[string]int{}
	for range 4000 {
		got, err := tbl.Choose("x", "404", "*")
		if err != nil {
			t.Fatal(err)
		}
		n[got.VendorID]++
	}
	if n["a"] < 2800 || n["a"] > 3200 {
		t.Fatalf("3:1 split off: %v", n)
	}
}

func TestVendorDeals(t *testing.T) {
	active := map[string]bool{"spot": true, "deal": true, "dealonly": true, "airtel": true}
	costs := map[string]map[string]decimal.Decimal{
		"spot":   {"404|*": d("0.05")},
		"deal":   {"404|*": d("0.09")},
		"airtel": {"404|*": d("0.07")},
	}
	now := time.Now()
	run := func(id, vendor, mnc, cost string, commit int64) VendorDeal {
		return VendorDeal{ID: id, VendorID: vendor, MCC: "404", MNC: mnc, Cost: d(cost), Commit: commit,
			Starts: now.Add(-time.Hour), Ends: now.Add(time.Hour)}
	}
	p := &Progress{}
	tbl := NewTable([]Rule{{MCC: "404", MNC: "*", Mode: "lcr", Priority: 1}}, active, costs).WithDeals([]VendorDeal{
		run("d1", "deal", "*", "0.06", 100), // dearer than spot, but committed
		{ID: "old", VendorID: "spot", MCC: "404", MNC: "*", Cost: d("0.01"), Commit: 1,
			Starts: now.Add(-2 * time.Hour), Ends: now.Add(-time.Hour)}, // ended: ignored
	}, p)

	got, _ := tbl.Choose("x", "404", "10")
	if got.VendorID != "deal" || got.DealID != "d1" || !got.Cost.Equal(d("0.06")) {
		t.Fatalf("unmet commitment must win at the deal cost: %+v", got)
	}
	p.Add("d1", 100)
	got, _ = tbl.Choose("x", "404", "10")
	if got.VendorID != "spot" || got.DealID != "" {
		t.Fatalf("met commitment: back to cheapest: %+v", got)
	}

	// The deal cost still counts after the commitment, and a network-specific
	// deal beats a '*' one; a deal needs no cost list entry.
	tbl = NewTable([]Rule{{MCC: "404", MNC: "*", Mode: "lcr", Priority: 1}}, active, costs).WithDeals([]VendorDeal{
		run("w", "airtel", "*", "0.08", 10),
		run("n", "airtel", "45", "0.02", 10),
		run("only", "dealonly", "*", "0.04", 10),
	}, &Progress{routed: map[string]int64{"w": 10, "n": 10, "only": 10}})
	if got, _ := tbl.Choose("x", "404", "45"); got.VendorID != "airtel" || got.DealID != "n" {
		t.Fatalf("network deal: %+v", got)
	}
	if got, _ := tbl.Choose("x", "404", "10"); got.VendorID != "dealonly" || !got.Cost.Equal(d("0.04")) {
		t.Fatalf("vendor priced only by a deal: %+v", got)
	}
}

func TestFailover(t *testing.T) {
	active := map[string]bool{"a": true, "b": true, "c": true}
	costs := map[string]map[string]decimal.Decimal{"a": {"404|*": d("0.01")}, "b": {"404|*": d("0.02")}, "c": {"404|*": d("0.03")}}
	h := &Health{}
	tbl := NewTable([]Rule{
		{MCC: "404", MNC: "*", Mode: "lcr", Priority: 1},
		{AccountID: "vip", MCC: "404", MNC: "*", Mode: "priority", VendorID: "a", Priority: 1},
		{AccountID: "vip", MCC: "404", MNC: "*", Mode: "priority", VendorID: "c", Priority: 2},
	}, active, costs).WithHealth(h)

	h.Set(map[string]vendorhealth.State{"a": {Up: false, Reason: "down"}, "b": {Up: true}})
	if got, _ := tbl.Choose("x", "404", "10"); got.VendorID != "b" {
		t.Fatalf("lcr skips a down vendor: %+v", got)
	}
	if got, _ := tbl.Choose("vip", "404", "10"); got.VendorID != "c" {
		t.Fatalf("priority falls over: %+v", got)
	}
	if got, _ := tbl.Choose("x", "404", "10", "b"); got.VendorID != "c" {
		t.Fatalf("avoid list: %+v", got)
	}
	// Nothing healthy left: queue at a down vendor rather than refuse.
	if got, err := tbl.Choose("x", "404", "10", "b", "c"); err != nil || got.VendorID != "a" {
		t.Fatalf("all others avoided: %+v %v", got, err)
	}
	h.Set(map[string]vendorhealth.State{"a": {Up: false}, "b": {Up: false}, "c": {Up: false}})
	if got, err := tbl.Choose("x", "404", "10"); err != nil || got.VendorID != "a" {
		t.Fatalf("all down: cheapest anyway: %+v %v", got, err)
	}
	// Every vendor avoided: still routed (the connector stops rerouting after MaxReroutes).
	if _, err := tbl.Choose("x", "404", "10", "a", "b", "c"); err != nil {
		t.Fatalf("all avoided: %v", err)
	}
}
