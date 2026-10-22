package main

import (
	"testing"
	"time"
)

func TestHistQuantiles(t *testing.T) {
	var h hist
	// 1..10,000 ms, once each: p50 is 5 s, p99 9.9 s.
	for i := 1; i <= 10000; i++ {
		h.record(time.Duration(i) * time.Millisecond)
	}
	s := h.snapshot()
	for _, c := range []struct {
		q    float64
		want time.Duration
	}{{0.5, 5 * time.Second}, {0.99, 9900 * time.Millisecond}, {1, 10 * time.Second}} {
		got := s.quantile(c.q)
		if got < c.want || float64(got) > float64(c.want)*1.0625 {
			t.Errorf("q%.2f = %s, want %s (+6.25%%)", c.q, got, c.want)
		}
	}
	if time.Duration(h.max.Load())*time.Microsecond != 10*time.Second {
		t.Errorf("max %d", h.max.Load())
	}
	// An interval: only what came after the earlier snapshot.
	for range 100 {
		h.record(50 * time.Microsecond)
	}
	d := h.snapshot().minus(s)
	if d.n != 100 || d.quantile(0.99) > 54*time.Microsecond {
		t.Errorf("interval: n=%d p99=%s", d.n, d.quantile(0.99))
	}
	for us := int64(0); us < 1<<20; us = us*3/2 + 1 {
		if b := bucketOf(us); lowerBound(b) > us || lowerBound(b+1) <= us {
			t.Fatalf("bucket %d of %d µs covers [%d, %d)", b, us, lowerBound(b), lowerBound(b+1))
		}
	}
}
