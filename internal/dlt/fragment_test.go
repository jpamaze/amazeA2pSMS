package dlt

import "testing"

func TestMatchFragment(t *testing.T) {
	c, err := Compile("Dear {#var#}, your account {#numeric#} is credited with Rs. {#numeric#} on {#var#}. Call {#cbn#} for help. -Bank")
	if err != nil {
		t.Fatal(err)
	}
	whole := "Dear Asha, your account 1234 is credited with Rs. 5,000.00 on 21-10-2026. Call 1800 123 4567 for help. -Bank"
	if _, ok := c.Match(whole); !ok {
		t.Fatal("whole message must match")
	}
	// Any split of a matching message fits, part by part.
	for _, cut := range []int{1, 7, 20, 33, 50, 61, 80, len(whole) - 1} {
		if !c.MatchFragment(whole[:cut]) || !c.MatchFragment(whole[cut:]) {
			t.Errorf("split at %d: %q | %q", cut, whole[:cut], whole[cut:])
		}
	}
	// Line breaks may replace spaces, as in Match.
	if !c.MatchFragment("credited with\nRs. 10") {
		t.Error("whitespace runs")
	}
	for _, bad := range []string{
		"Win a free iPhone now, click bit.ly/x",          // unrelated text
		"your account 1234 is debited with Rs. 5,000",    // a literal word changed (30+ characters cannot hide in one {#var#})
		"Dear Asha, your account 12AB is credited",       // non-numeric in a numeric placeholder
		"Dear aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa, your", // placeholder longer than 30
		"-Bank and more text after the end",              // beyond the end of the template
	} {
		if c.MatchFragment(bad) {
			t.Errorf("fragment should not fit: %q", bad)
		}
	}
}
