package numbering

import "testing"

func TestNormalizeIndia(t *testing.T) {
	good := map[string]string{
		"+91 98123 45678": "919812345678",
		"919812345678":    "919812345678",
		"09812345678":     "919812345678",
		"9812345678":      "919812345678",
		"0091-6012345678": "916012345678",
	}
	for in, want := range good {
		d, err := NormalizeIndia(in)
		if err != nil || d.E164 != want || d.MCC != "404" {
			t.Errorf("%q -> %+v %v", in, d, err)
		}
	}
	for _, bad := range []string{"", "5812345678", "98123456", "+14155550100", "98123x5678", "9198123456789"} {
		if _, err := NormalizeIndia(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestValidateSender(t *testing.T) {
	for _, ok := range []string{"AMZTST", "Amaze1", "56767", "919812345678"} {
		if err := ValidateSender(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "TOOLONGHEADER", "AB-CD", "12", "1234567890123456"} {
		if err := ValidateSender(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
