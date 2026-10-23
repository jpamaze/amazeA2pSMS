package adminapi

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestPeriodTimeZone(t *testing.T) {
	get := func(q string) (time.Time, time.Time, error) {
		return period(httptest.NewRequest("GET", "/x?"+q, nil))
	}
	from, to, err := get("from=2026-10-01&to=2026-11-01")
	if err != nil || !from.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("default UTC: %v %v %v", from, to, err)
	}
	from, _, err = get("from=2026-10-01&to=2026-11-01&tz=Asia/Kolkata")
	if err != nil || !from.Equal(time.Date(2026, 9, 30, 18, 30, 0, 0, time.UTC)) || from.Location().String() != "Asia/Kolkata" {
		t.Fatalf("IST: %v %v", from, err)
	}
	from, _, err = get("from=2026-10-01T10:00:00Z&to=2026-10-02&tz=America/New_York")
	if err != nil || !from.Equal(time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("RFC 3339 keeps its own offset: %v %v", from, err)
	}
	for _, bad := range []string{"tz=Mars/Base", "tz=UTC%27%3BDROP", "tz=../../etc/passwd", "tz=Asia/Kolkata&from=2026-10-02&to=2026-10-01"} {
		if _, _, err := get(bad); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
	if _, _, err := get("tz=Asia/Kolkata"); err != nil {
		t.Fatalf("current month in IST: %v", err)
	}
}
