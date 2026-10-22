package main

import (
	"math/bits"
	"sync/atomic"
	"time"
)

// hist is a lock-free latency histogram with fixed memory, so a 24-hour
// soak at 10,000 TPS (864 million samples) costs a few kilobytes. Buckets
// are log-linear: 16 per power of two of microseconds, under 6.25% error.
type hist struct {
	counts [64 * subBuckets]atomic.Int64
	n      atomic.Int64
	max    atomic.Int64 // microseconds
}

const subBuckets = 16

func bucketOf(us int64) int {
	if us < subBuckets {
		return int(max(us, 0))
	}
	p := bits.Len64(uint64(us)) - 1 // us in [2^p, 2^(p+1))
	sub := int((us >> (p - 4)) & (subBuckets - 1))
	return (p-3)*subBuckets + sub
}

// lowerBound is the smallest value (µs) in bucket i.
func lowerBound(i int) int64 {
	if i < subBuckets {
		return int64(i)
	}
	p := i/subBuckets + 3
	sub := int64(i % subBuckets)
	return (1 << p) + sub<<(p-4)
}

func (h *hist) record(d time.Duration) {
	us := d.Microseconds()
	h.counts[bucketOf(us)].Add(1)
	h.n.Add(1)
	for {
		m := h.max.Load()
		if us <= m || h.max.CompareAndSwap(m, us) {
			return
		}
	}
}

// snap is a point-in-time copy of the counts.
type snap struct {
	counts [64 * subBuckets]int64
	n      int64
}

func (h *hist) snapshot() snap {
	var s snap
	for i := range h.counts {
		s.counts[i] = h.counts[i].Load()
	}
	s.n = h.n.Load()
	return s
}

// minus gives the samples recorded between an earlier snapshot and s.
func (s snap) minus(earlier snap) snap {
	var d snap
	for i := range s.counts {
		d.counts[i] = s.counts[i] - earlier.counts[i]
	}
	d.n = s.n - earlier.n
	return d
}

// quantile returns the upper end of the bucket holding quantile q.
func (s snap) quantile(q float64) time.Duration {
	if s.n == 0 {
		return 0
	}
	rank := int64(q*float64(s.n-1)) + 1
	var seen int64
	for i, c := range s.counts {
		seen += c
		if seen >= rank {
			return time.Duration(lowerBound(i+1)) * time.Microsecond
		}
	}
	return 0
}
