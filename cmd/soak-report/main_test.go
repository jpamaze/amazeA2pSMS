package main

import (
	"strings"
	"testing"
	"time"
)

// fake answers by query prefix, early or late.
type fake struct {
	early, late time.Time
	vec         map[string][2]map[string]float64 // query prefix -> [early, late]
	scal        map[string]float64
}

func (f fake) vector(q string, at time.Time) (map[string]float64, error) {
	for p, v := range f.vec {
		if strings.Contains(q, p) {
			if at.Equal(f.early) {
				return v[0], nil
			}
			return v[1], nil
		}
	}
	return map[string]float64{}, nil
}

func (f fake) scalar(q string, at time.Time) (float64, error) {
	for p, v := range f.scal {
		if strings.Contains(q, p) {
			return v, nil
		}
	}
	return 0, nil
}

func TestSoakEvaluate(t *testing.T) {
	early, late := time.Unix(1_000_000, 0), time.Unix(1_080_000, 0)
	base := func() fake {
		return fake{early: early, late: late,
			vec: map[string][2]map[string]float64{
				"process_resident_memory_bytes": {{"app-1:9101": 200 << 20, "app-1:9105": 100 << 20}, {"app-1:9101": 220 << 20, "app-1:9105": 110 << 20}},
				"go_goroutines":                 {{"app-1:9101": 900}, {"app-1:9101": 950}},
				"sms_final_total":               {{"platform": 40000}, {"platform": 42000}},
			},
			scal: map[string]float64{"sms_accepted_total[": 864e6}}
	}
	lim := limits{0.25, 0.5, 0.5, 0}
	if r := evaluate(base(), early, late, 24*time.Hour, time.Hour, lim); !r.Pass {
		t.Fatalf("flat run should pass: %v", r.Failures)
	}

	leak := base()
	leak.vec["process_resident_memory_bytes"] = [2]map[string]float64{{"app-1:9104": 300 << 20}, {"app-1:9104": 900 << 20}}
	if r := evaluate(leak, early, late, 24*time.Hour, time.Hour, lim); r.Pass || !strings.Contains(strings.Join(r.Failures, " "), "app-1:9104") {
		t.Fatalf("memory leak: %v", r.Failures)
	}
	backlog := base()
	backlog.vec["sms_final_total"] = [2]map[string]float64{{"platform": 40000}, {"platform": 400000}}
	if r := evaluate(backlog, early, late, 24*time.Hour, time.Hour, lim); r.Pass {
		t.Fatal("growing backlog must fail")
	}
	restart := base()
	restart.scal["process_start_time_seconds"] = 1
	if r := evaluate(restart, early, late, 24*time.Hour, time.Hour, lim); r.Pass {
		t.Fatal("a restart must fail")
	}
	// Small absolute growth of a small number is noise, not a leak.
	small := base()
	small.vec["go_goroutines"] = [2]map[string]float64{{"app-1:9106": 40}, {"app-1:9106": 90}}
	if r := evaluate(small, early, late, 24*time.Hour, time.Hour, lim); !r.Pass {
		t.Fatalf("small growth: %v", r.Failures)
	}
}
