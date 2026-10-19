package gsm7

import (
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	for _, s := range []string{"Hello @ £5 {ok} €", "OTP 123456 for login", "àÄ_\n[x]~|^\\"} {
		enc, err := Encode(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if got := Decode(enc); got != s {
			t.Fatalf("round trip: %q -> %q", s, got)
		}
	}
	if _, err := Encode("नमस्ते"); err != ErrNotGSM {
		t.Fatalf("Devanagari must not be GSM, got %v", err)
	}
	if got := DecodeUCS2(EncodeUCS2("नमस्ते 😀")); got != "नमस्ते 😀" {
		t.Fatalf("ucs2 round trip: %q", got)
	}
}

func TestPlan(t *testing.T) {
	cases := []struct {
		s      string
		coding Coding
		units  int
		segs   int
	}{
		{strings.Repeat("a", 160), CodingGSM7, 160, 1},
		{strings.Repeat("a", 161), CodingGSM7, 161, 2},
		{strings.Repeat("a", 306), CodingGSM7, 306, 2},
		{strings.Repeat("a", 307), CodingGSM7, 307, 3},
		{strings.Repeat("€", 80), CodingGSM7, 160, 1}, // extension chars count twice
		{strings.Repeat("क", 70), CodingUCS2, 70, 1},
		{strings.Repeat("क", 71), CodingUCS2, 71, 2},
	}
	for _, c := range cases {
		p := PlanFor(c.s)
		if p.Coding != c.coding || p.Units != c.units || p.Segments != c.segs {
			t.Errorf("PlanFor(len %d): got %+v, want coding %d units %d segs %d", len(c.s), p, c.coding, c.units, c.segs)
		}
	}
}

func TestSplitKeepsPairsTogether(t *testing.T) {
	// 152 'a' then '€' (escape pair) lands on the 153 boundary: the pair must move whole.
	s := strings.Repeat("a", 152) + "€" + strings.Repeat("b", 20)
	coding, parts := Split(s)
	if coding != CodingGSM7 || len(parts) != 2 {
		t.Fatalf("got coding %d, %d parts", coding, len(parts))
	}
	if len(parts[0]) != 152 || Decode(parts[0])+Decode(parts[1]) != s {
		t.Fatalf("escape split wrongly: first part %d septets", len(parts[0]))
	}

	u := strings.Repeat("क", 66) + "😀" + strings.Repeat("क", 5)
	coding, parts = Split(u)
	if coding != CodingUCS2 || len(parts) != 2 {
		t.Fatalf("ucs2: coding %d, %d parts", coding, len(parts))
	}
	if DecodeUCS2(parts[0])+DecodeUCS2(parts[1]) != u || len(parts[0]) != 132 {
		t.Fatalf("surrogate split wrongly: first part %d bytes", len(parts[0]))
	}
}
