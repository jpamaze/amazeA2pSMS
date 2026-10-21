package recon

import (
	"testing"

	"github.com/shopspring/decimal"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestCompare(t *testing.T) {
	vendor := []Line{
		{Day: "2026-10-01", MCC: "404", MNC: "10", Segments: 1000, Amount: dec("50.00")}, // matches
		{Day: "2026-10-01", MCC: "404", MNC: "45", Segments: 1300, Amount: dec("65.00")}, // 300 too many
		{Day: "2026-10-02", MCC: "405", MNC: "*", Segments: 2002, Amount: dec("100.10")}, // country-level, in tolerance
		{Day: "2026-10-03", MCC: "404", MNC: "10", Segments: 10, Amount: dec("0.50")},    // we have nothing
	}
	ours := []Line{
		{Day: "2026-10-01", MCC: "404", MNC: "10", Segments: 1000, Amount: dec("50.00")},
		{Day: "2026-10-01", MCC: "404", MNC: "45", Segments: 1000, Amount: dec("50.00")},
		{Day: "2026-10-02", MCC: "405", MNC: "01", Segments: 1500, Amount: dec("75.00")},
		{Day: "2026-10-02", MCC: "405", MNC: "02", Segments: 500, Amount: dec("25.00")},
		{Day: "2026-10-04", MCC: "404", MNC: "10", Segments: 20, Amount: dec("1.00")}, // vendor did not bill
	}
	diffs, s := Compare(vendor, ours, dec("0.5"))
	want := map[string]string{
		"2026-10-01|404|10": OK, "2026-10-01|404|45": Mismatch, "2026-10-02|405|*": OK,
		"2026-10-03|404|10": NotInOurs, "2026-10-04|404|10": NotInVendor,
	}
	if len(diffs) != len(want) {
		t.Fatalf("lines: %+v", diffs)
	}
	for _, d := range diffs {
		if w := want[d.Day+"|"+d.MCC+"|"+d.MNC]; w != d.Status {
			t.Errorf("%s %s/%s: want %s, got %s", d.Day, d.MCC, d.MNC, w, d.Status)
		}
	}
	// Disputable: 15.00 overbilled on 404/45 plus 0.50 we have no record of.
	if !s.Disputable.Equal(dec("15.50")) || s.Mismatched != 3 {
		t.Fatalf("summary: %+v", s)
	}
	if s.VendorSegments != 4312 || s.OurSegments != 4020 {
		t.Fatalf("totals: %+v", s)
	}
}
