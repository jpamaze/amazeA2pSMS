package routing

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
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
