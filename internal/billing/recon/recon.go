// Package recon compares what a vendor billed with what we sent through it,
// per day and destination, to find lines worth disputing.
package recon

import (
	"sort"

	"github.com/shopspring/decimal"
)

// Line is traffic for one day (IST, YYYY-MM-DD) and destination. MNC '*'
// means the whole country.
type Line struct {
	Day      string          `json:"day"`
	MCC      string          `json:"mcc"`
	MNC      string          `json:"mnc"`
	Segments int64           `json:"segments"`
	Amount   decimal.Decimal `json:"amount"`
}

// Line statuses.
const (
	OK          = "ok"
	Mismatch    = "mismatch"      // both sides have it, outside tolerance
	NotInOurs   = "not_in_ours"   // vendor billed traffic we have no record of
	NotInVendor = "not_in_vendor" // we sent it, the vendor did not bill it
)

// Diff is one compared line.
type Diff struct {
	Day            string          `json:"day"`
	MCC            string          `json:"mcc"`
	MNC            string          `json:"mnc"`
	VendorSegments int64           `json:"vendor_segments"`
	OurSegments    int64           `json:"our_segments"`
	VendorAmount   decimal.Decimal `json:"vendor_amount"`
	OurAmount      decimal.Decimal `json:"our_amount"`
	Overbilled     decimal.Decimal `json:"overbilled"` // vendor amount above ours, never negative
	Status         string          `json:"status"`
}

// Summary totals a comparison.
type Summary struct {
	VendorSegments int64           `json:"vendor_segments"`
	OurSegments    int64           `json:"our_segments"`
	VendorAmount   decimal.Decimal `json:"vendor_amount"`
	OurAmount      decimal.Decimal `json:"our_amount"`
	Disputable     decimal.Decimal `json:"disputable"` // overbilled on lines outside tolerance
	Mismatched     int             `json:"mismatched_lines"`
}

type key struct{ day, mcc, mnc string }

// Compare matches vendor lines with ours. Where the vendor bills a day and
// country as a whole (MNC '*'), our networks are summed to match. A line is
// out of tolerance when segments or amount differ by more than tolPct
// percent of the vendor's figure (and the amount by more than 0.01).
func Compare(vendor, ours []Line, tolPct decimal.Decimal) ([]Diff, Summary) {
	whole := map[[2]string]bool{} // day|mcc billed per country
	for _, l := range vendor {
		if l.MNC == "*" {
			whole[[2]string{l.Day, l.MCC}] = true
		}
	}
	v := map[key]*Diff{}
	get := func(l Line) *Diff {
		mnc := l.MNC
		if whole[[2]string{l.Day, l.MCC}] {
			mnc = "*"
		}
		k := key{l.Day, l.MCC, mnc}
		if v[k] == nil {
			v[k] = &Diff{Day: l.Day, MCC: l.MCC, MNC: mnc}
		}
		return v[k]
	}
	for _, l := range vendor {
		d := get(l)
		d.VendorSegments += l.Segments
		d.VendorAmount = d.VendorAmount.Add(l.Amount)
	}
	for _, l := range ours {
		d := get(l)
		d.OurSegments += l.Segments
		d.OurAmount = d.OurAmount.Add(l.Amount)
	}

	hundred := decimal.NewFromInt(100)
	cent := decimal.New(1, -2)
	var out []Diff
	var s Summary
	for _, d := range v {
		switch {
		case d.OurSegments == 0 && d.OurAmount.IsZero():
			d.Status = NotInOurs
		case d.VendorSegments == 0 && d.VendorAmount.IsZero():
			d.Status = NotInVendor
		default:
			segTol := decimal.NewFromInt(d.VendorSegments).Mul(tolPct).Div(hundred)
			amtTol := decimal.Max(d.VendorAmount.Mul(tolPct).Div(hundred), cent)
			segOff := decimal.NewFromInt(d.VendorSegments - d.OurSegments).Abs().GreaterThan(segTol)
			amtOff := d.VendorAmount.Sub(d.OurAmount).Abs().GreaterThan(amtTol)
			d.Status = OK
			if segOff || amtOff {
				d.Status = Mismatch
			}
		}
		d.Overbilled = decimal.Max(d.VendorAmount.Sub(d.OurAmount), decimal.Zero)
		if d.Status == Mismatch || d.Status == NotInOurs {
			s.Disputable = s.Disputable.Add(d.Overbilled)
		}
		if d.Status != OK {
			s.Mismatched++
		}
		s.VendorSegments += d.VendorSegments
		s.OurSegments += d.OurSegments
		s.VendorAmount = s.VendorAmount.Add(d.VendorAmount)
		s.OurAmount = s.OurAmount.Add(d.OurAmount)
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Day != b.Day {
			return a.Day < b.Day
		}
		if a.MCC != b.MCC {
			return a.MCC < b.MCC
		}
		return a.MNC < b.MNC
	})
	return out, s
}
