// Package vendorhealth takes a vendor out of routing when it goes down,
// slows, or its delivery rate drops (blueprint reliability rule 8), and puts
// it back when it recovers.
//
// Connectors report what they see every two seconds (binds up, submit
// outcomes and latency over the last 30 seconds). The DLR processor counts
// final outcomes per vendor per minute. Routers evaluate those signals
// against each vendor's policy every two seconds; every router reaches the
// same verdict from the same data, and the verdict is published for
// connectors (which then reroute queued traffic) and for operators.
package vendorhealth

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	healthKey = "vhealth" // hash vendor|instance -> report JSON
	stateKey  = "vstate"  // hash vendor -> state JSON
	window    = 30        // seconds of submit history in a report
	stale     = 10 * time.Second
)

func dlrKey(vendor string, minute time.Time) string {
	return fmt.Sprintf("vdlr:{%s}:%s", vendor, minute.UTC().Format("200601021504"))
}

// Report is one connector instance's view of its vendor.
type Report struct {
	At       int64 `json:"at"` // unix ms
	BindsUp  int   `json:"binds_up"`
	Binds    int   `json:"binds"` // 0 for HTTP vendors (no binds)
	OK       int   `json:"ok"`
	Failed   int   `json:"failed"` // temporary failures: timeouts, unreachable, throttled
	P95Ms    int   `json:"p95_ms"`
	Rerouted int   `json:"rerouted"`
}

// Recorder collects submit outcomes in one-second buckets.
type Recorder struct {
	mu      sync.Mutex
	buckets [window]bucket
}

type bucket struct {
	sec        int64
	ok, failed int
	rerouted   int
	latencies  []time.Duration
}

func (r *Recorder) at(now time.Time) *bucket {
	s := now.Unix()
	b := &r.buckets[s%window]
	if b.sec != s {
		*b = bucket{sec: s}
	}
	return b
}

// Submit records one submit's outcome. temporary marks a failure that says
// something about the vendor's health (not a refusal of the message).
func (r *Recorder) Submit(d time.Duration, err error, temporary bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.at(time.Now())
	switch {
	case err == nil:
		b.ok++
		if len(b.latencies) < 2000 {
			b.latencies = append(b.latencies, d)
		}
	case temporary:
		b.failed++
	default:
		b.ok++ // the vendor answered; it refused this message
	}
}

// Rerouted counts a message handed back for another route.
func (r *Recorder) Rerouted() {
	r.mu.Lock()
	r.at(time.Now()).rerouted++
	r.mu.Unlock()
}

// Snapshot summarises the last window.
func (r *Recorder) Snapshot(bindsUp, binds int) Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	rep := Report{At: now.UnixMilli(), BindsUp: bindsUp, Binds: binds}
	var lat []time.Duration
	for i := range r.buckets {
		b := &r.buckets[i]
		if now.Unix()-b.sec >= window {
			continue
		}
		rep.OK += b.ok
		rep.Failed += b.failed
		rep.Rerouted += b.rerouted
		lat = append(lat, b.latencies...)
	}
	if len(lat) > 0 {
		slices.Sort(lat)
		rep.P95Ms = int(lat[len(lat)*95/100].Milliseconds())
	}
	return rep
}

// Publish writes a connector instance's report.
func Publish(ctx context.Context, rdb redis.UniversalClient, vendor, instance string, rep Report) error {
	b, _ := json.Marshal(rep)
	return rdb.HSet(ctx, healthKey, vendor+"|"+instance, b).Err()
}

// CountDLRs adds final outcomes per vendor (vendor -> [finals, delivered])
// to the current minute.
func CountDLRs(ctx context.Context, rdb redis.UniversalClient, counts map[string][2]int) error {
	if len(counts) == 0 {
		return nil
	}
	now := time.Now()
	_, err := rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for v, c := range counts {
			k := dlrKey(v, now)
			p.HIncrBy(ctx, k, "final", int64(c[0]))
			p.HIncrBy(ctx, k, "delivered", int64(c[1]))
			p.Expire(ctx, k, 2*time.Hour)
		}
		return nil
	})
	return err
}

// Policy is when a vendor is taken out of routing.
type Policy struct {
	Enabled     bool
	MaxSubmitMs int           // p95 submit latency above this = slow (0 = off)
	MaxErrorPct int           // temporary failures above this share of submits (0 = off)
	MinDLRPct   int           // delivered share of final receipts below this (0 = off)
	DLRWindow   time.Duration // receipts window for MinDLRPct
	Cooldown    time.Duration // minimum time out of routing before a retry
}

// Minimum samples before latency, error or DLR rules act.
const (
	minSubmits = 20
	minFinals  = 200
)

// State is a vendor's routing verdict.
type State struct {
	Up     bool   `json:"up"`
	Reason string `json:"reason,omitempty"` // down | slow | errors | dlr_rate
	Since  int64  `json:"since"`            // unix ms of the last change
	Detail string `json:"detail,omitempty"`
}

// Signals are the inputs to a verdict.
type Signals struct {
	Reports   []Report // fresh reports from connector instances
	Finals    int
	Delivered int
}

// Decide applies the policy. prev is the last verdict; now is the time of
// evaluation.
func Decide(p Policy, sig Signals, prev State, now time.Time) State {
	if !p.Enabled {
		return keep(prev, State{Up: true}, now)
	}
	var up, binds, ok, failed, p95 int
	for _, r := range sig.Reports {
		up += r.BindsUp
		binds += r.Binds
		ok += r.OK
		failed += r.Failed
		p95 = max(p95, r.P95Ms)
	}
	var bad State
	switch {
	case len(sig.Reports) == 0:
		bad = State{Reason: "down", Detail: "no connector reporting"}
	case binds > 0 && up == 0:
		bad = State{Reason: "down", Detail: fmt.Sprintf("0 of %d binds up", binds)}
	case p.MaxErrorPct > 0 && ok+failed >= minSubmits && failed*100 > p.MaxErrorPct*(ok+failed):
		bad = State{Reason: "errors", Detail: fmt.Sprintf("%d of %d submits failed in %ds", failed, ok+failed, window)}
	case p.MaxSubmitMs > 0 && ok >= minSubmits && p95 > p.MaxSubmitMs:
		bad = State{Reason: "slow", Detail: fmt.Sprintf("p95 submit %d ms > %d ms", p95, p.MaxSubmitMs)}
	case p.MinDLRPct > 0 && sig.Finals >= minFinals && sig.Delivered*100 < p.MinDLRPct*sig.Finals:
		bad = State{Reason: "dlr_rate", Detail: fmt.Sprintf("%d of %d delivered", sig.Delivered, sig.Finals)}
	}
	if bad.Reason != "" {
		return keep(prev, bad, now)
	}
	// Healthy again. Out-of-routing vendors get no traffic and so no submit
	// samples; the cooldown stops a down vendor flapping straight back.
	// Binds coming back is direct evidence, so "down" ends at once.
	if !prev.Up && prev.Since > 0 && prev.Reason != "down" && now.Sub(time.UnixMilli(prev.Since)) < p.Cooldown {
		return prev
	}
	return keep(prev, State{Up: true}, now)
}

func keep(prev, next State, now time.Time) State {
	if prev.Up == next.Up && prev.Reason == next.Reason && prev.Since > 0 {
		next.Since = prev.Since
		return next
	}
	next.Since = now.UnixMilli()
	return next
}

// Evaluate reads every vendor's signals, decides, and publishes the
// verdicts. It returns vendor -> state.
func Evaluate(ctx context.Context, rdb redis.UniversalClient, policies map[string]Policy, prev map[string]State) (map[string]State, error) {
	now := time.Now()
	raw, err := rdb.HGetAll(ctx, healthKey).Result()
	if err != nil {
		return nil, err
	}
	reports := map[string][]Report{}
	var expired []string
	for field, v := range raw {
		vendor, _, _ := cut(field)
		var r Report
		if json.Unmarshal([]byte(v), &r) != nil || now.Sub(time.UnixMilli(r.At)) > stale {
			if now.Sub(time.UnixMilli(r.At)) > time.Hour {
				expired = append(expired, field) // instance gone for good
			}
			continue
		}
		reports[vendor] = append(reports[vendor], r)
	}
	if len(expired) > 0 {
		rdb.HDel(ctx, healthKey, expired...)
	}

	// Receipt counts over each vendor's window, one pipeline.
	type dlrCmd struct {
		vendor string
		cmds   []*redis.SliceCmd
	}
	var dcs []dlrCmd
	_, err = rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for v, pol := range policies {
			if pol.MinDLRPct == 0 || !pol.Enabled {
				continue
			}
			mins := max(int(pol.DLRWindow/time.Minute), 1)
			dc := dlrCmd{vendor: v}
			for i := range mins {
				dc.cmds = append(dc.cmds, p.HMGet(ctx, dlrKey(v, now.Add(-time.Duration(i)*time.Minute)), "final", "delivered"))
			}
			dcs = append(dcs, dc)
		}
		return nil
	})
	if err != nil && err != redis.Nil {
		return nil, err
	}
	finals := map[string][2]int{}
	for _, dc := range dcs {
		var f, d int
		for _, c := range dc.cmds {
			vals, _ := c.Result()
			if len(vals) == 2 {
				f += atoi(vals[0])
				d += atoi(vals[1])
			}
		}
		finals[dc.vendor] = [2]int{f, d}
	}

	out := make(map[string]State, len(policies))
	pub := map[string]any{}
	for v, pol := range policies {
		st := Decide(pol, Signals{Reports: reports[v], Finals: finals[v][0], Delivered: finals[v][1]}, prev[v], now)
		out[v] = st
		b, _ := json.Marshal(st)
		pub[v] = b
	}
	if len(pub) > 0 {
		if err := rdb.HSet(ctx, stateKey, pub).Err(); err != nil {
			return out, err
		}
	}
	return out, nil
}

// States reads the published verdicts (connectors and the admin API).
func States(ctx context.Context, rdb redis.UniversalClient) (map[string]State, error) {
	raw, err := rdb.HGetAll(ctx, stateKey).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]State, len(raw))
	for v, s := range raw {
		var st State
		if json.Unmarshal([]byte(s), &st) == nil {
			out[v] = st
		}
	}
	return out, nil
}

// Reports returns fresh connector reports per vendor (admin API).
func Reports(ctx context.Context, rdb redis.UniversalClient) (map[string][]Report, error) {
	raw, err := rdb.HGetAll(ctx, healthKey).Result()
	if err != nil {
		return nil, err
	}
	out := map[string][]Report{}
	for field, v := range raw {
		vendor, _, _ := cut(field)
		var r Report
		if json.Unmarshal([]byte(v), &r) == nil && time.Since(time.UnixMilli(r.At)) <= stale {
			out[vendor] = append(out[vendor], r)
		}
	}
	return out, nil
}

func cut(field string) (string, string, bool) {
	for i := len(field) - 1; i >= 0; i-- {
		if field[i] == '|' {
			return field[:i], field[i+1:], true
		}
	}
	return field, "", false
}

func atoi(v any) int {
	s, _ := v.(string)
	n, _ := strconv.Atoi(s)
	return n
}
