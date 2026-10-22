// Command smpp-load binds to the platform as a customer, sends messages at a
// fixed rate and reports throughput, latency and delivery receipts. With
// thresholds set it is the pass/fail gate of the load-test sign-off.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/session"
)

type report struct {
	Started         time.Time          `json:"started"`
	TargetTPS       int                `json:"target_tps"`
	DurationSeconds float64            `json:"duration_seconds"`
	Accepted        int64              `json:"accepted"`
	Failed          int64              `json:"failed"`
	FailedByStatus  map[string]int     `json:"failed_by_status,omitempty"`
	AcceptedTPS     float64            `json:"accepted_tps"`
	LatencyMs       map[string]float64 `json:"submit_resp_latency_ms"`
	Receipts        int64              `json:"receipts"`
	Delivered       int64              `json:"delivered"`
	ReceiptLagSec   float64            `json:"receipt_lag_seconds"` // after sending stopped
	Intervals       []interval         `json:"intervals,omitempty"`
	Reconnects      int64              `json:"reconnects,omitempty"`
	Pass            bool               `json:"pass"`
	Failures        []string           `json:"failures,omitempty"`
}

// interval is one checkpoint of a long run (-interval).
type interval struct {
	End         time.Time `json:"end"`
	Accepted    int64     `json:"accepted"`
	Failed      int64     `json:"failed"`
	AcceptedTPS float64   `json:"accepted_tps"`
	P50Ms       float64   `json:"p50_ms"`
	P99Ms       float64   `json:"p99_ms"`
	Receipts    int64     `json:"receipts"`
	Outstanding int64     `json:"outstanding"` // accepted so far without a receipt yet
}

func main() {
	addrs := flag.String("addr", "localhost:2775", "platform SMPP address(es), comma-separated; binds are spread over them")
	user := flag.String("user", "demo", "system_id")
	pass := flag.String("pass", "demo123", "password")
	binds := flag.Int("binds", 4, "transceiver binds (total)")
	window := flag.Int("window", 100, "outstanding submits per bind")
	tps := flag.Int("tps", 1000, "target submits per second (total)")
	dur := flag.Duration("duration", 30*time.Second, "how long to send")
	drain := flag.Duration("drain", 30*time.Second, "how long to wait for receipts after sending")
	sender := flag.String("sender", "AMZTST", "sender ID")
	text := flag.String("text", "Your OTP is 482913. Do not share it with anyone. -Amaze", "message text")
	progress := flag.Duration("progress", 0, "print a progress line at this interval (0 = off)")
	reportPath := flag.String("report", "", "write a JSON report to this file")
	minRatio := flag.Float64("min-tps-ratio", 0, "fail if accepted TPS < ratio x target (e.g. 0.98)")
	maxP99 := flag.Duration("max-p99", 0, "fail if submit_sm_resp p99 exceeds this")
	maxFail := flag.Int64("max-failed", -1, "fail if more submits than this are refused (-1 = no limit)")
	maxLag := flag.Duration("max-receipt-lag", 0, "fail if the last receipt arrives later than this after sending stops")
	every := flag.Duration("interval", 0, "long runs: record a checkpoint (TPS, p99, receipts) at this interval; -max-p99 then applies to every interval")
	maxOutstanding := flag.Int64("max-outstanding", 0, "fail if, at any checkpoint, more accepted messages than this still wait for a receipt (0 = no limit)")
	reconnect := flag.Bool("reconnect", false, "re-bind a session that drops instead of losing it (soak tests)")
	flag.Parse()

	var (
		ok, fail, receipts, delivered atomic.Int64
		statusMu                      sync.Mutex
		statuses                      = map[uint32]int{}
		lat                           hist
		reconnects                    atomic.Int64
		sent                          sync.Map // message ID -> struct{}
		early                         sync.Map // receipts that beat their submit_sm_resp
		lastReceipt                   atomic.Int64
	)
	countReceipt := func(stat string) {
		receipts.Add(1)
		lastReceipt.Store(time.Now().UnixNano())
		if stat == "DELIVRD" {
			delivered.Add(1)
		}
	}

	handler := func(s *session.Session, req *pdu.PDU) {
		if req.CommandID == pdu.DeliverSM {
			sm := req.Body.(*pdu.SM)
			if sm.IsReceipt() {
				if r, err := pdu.ParseReceipt(sm); err == nil {
					if _, loaded := sent.LoadAndDelete(r.ID); loaded {
						countReceipt(r.Stat)
					} else {
						early.Store(r.ID, r.Stat)
					}
				}
			}
			_ = s.Respond(req, pdu.StatusOK, &pdu.SMResp{})
			return
		}
		_ = s.Respond(req, pdu.StatusInvCmdID, nil)
	}

	targets := strings.Split(*addrs, ",")
	bind := func(addr string) (*session.Session, error) {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			return nil, err
		}
		s := session.New(conn, session.Config{Window: *window, Handler: handler, MaxInflight: 256})
		s.Start()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := s.Send(ctx, pdu.BindTransceiver, &pdu.Bind{SystemID: *user, Password: *pass, InterfaceVersion: pdu.V34}); err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	}
	sessions := make([]atomic.Pointer[session.Session], *binds)
	for i := range *binds {
		addr := strings.TrimSpace(targets[i%len(targets)])
		s, err := bind(addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bind %d (%s): %v\n", i, addr, err)
			os.Exit(1)
		}
		sessions[i].Store(s)
		if *reconnect {
			go func() {
				for {
					<-sessions[i].Load().Done()
					for wait := time.Second; ; wait = min(wait*2, 30*time.Second) {
						if s, err := bind(addr); err == nil {
							sessions[i].Store(s)
							reconnects.Add(1)
							fmt.Printf("bind %d (%s) re-established\n", i, addr)
							break
						}
						time.Sleep(wait)
					}
				}
			}()
		}
	}
	fmt.Printf("bound %d sessions over %d address(es); sending %d TPS for %s\n", *binds, len(targets), *tps, *dur)

	lim := rate.NewLimiter(rate.Limit(*tps), max(1, *tps/50))
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	start := time.Now()

	if *progress > 0 {
		go func() {
			tick := time.NewTicker(*progress)
			defer tick.Stop()
			var lastOK, lastRc int64
			for {
				select {
				case <-tick.C:
				case <-time.After(*dur + *drain + time.Minute):
					return
				}
				o, r := ok.Load(), receipts.Load()
				fmt.Printf("[%5.0fs] accepted %d (%.0f/s) failed %d receipts %d (%.0f/s) outstanding %d\n",
					time.Since(start).Seconds(), o, float64(o-lastOK)/progress.Seconds(), fail.Load(),
					r, float64(r-lastRc)/progress.Seconds(), o-r)
				lastOK, lastRc = o, r
			}
		}()
	}

	var (
		intervalsMu sync.Mutex
		intervals   []interval
	)
	if *every > 0 {
		go func() {
			prev, prevOK, prevFail, prevRc := lat.snapshot(), int64(0), int64(0), int64(0)
			tick := time.NewTicker(*every)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-tick.C:
					cur, o, f, r := lat.snapshot(), ok.Load(), fail.Load(), receipts.Load()
					d := cur.minus(prev)
					iv := interval{End: now.UTC(), Accepted: o - prevOK, Failed: f - prevFail, AcceptedTPS: float64(o-prevOK) / every.Seconds(),
						P50Ms: ms(d.quantile(0.50)), P99Ms: ms(d.quantile(0.99)), Receipts: r - prevRc, Outstanding: o - r}
					fmt.Printf("checkpoint %s: accepted %d (%.0f/s) failed %d p50 %.1fms p99 %.1fms receipts %d outstanding %d reconnects %d\n",
						iv.End.Format(time.RFC3339), iv.Accepted, iv.AcceptedTPS, iv.Failed, iv.P50Ms, iv.P99Ms, iv.Receipts, iv.Outstanding, reconnects.Load())
					intervalsMu.Lock()
					intervals = append(intervals, iv)
					intervalsMu.Unlock()
					prev, prevOK, prevFail, prevRc = cur, o, f, r
				}
			}
		}()
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, *binds**window)
	var n uint64
	for ctx.Err() == nil {
		if lim.Wait(ctx) != nil {
			break
		}
		sem <- struct{}{}
		n++
		s := sessions[n%uint64(len(sessions))].Load()
		// Spread over the range like real traffic (7919 is coprime with 10^8,
		// so each number recurs only every 10^8 messages): sequential numbers
		// would trip the firewall's SMS-pumping guard.
		dest := fmt.Sprintf("9198%08d", (n*7919)%100000000)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			t0 := time.Now()
			sctx, c := context.WithTimeout(context.Background(), 15*time.Second)
			defer c()
			sm := &pdu.SM{Source: *sender, SourceTON: 5, Dest: dest, DestTON: 1, DestNPI: 1,
				RegisteredDelivery: 1, ShortMessage: []byte(*text)}
			sm.TLVs.SetString(0x1400, "1101234567890123456")
			sm.TLVs.SetString(0x1401, "1107160000000012345")
			resp, err := s.Send(sctx, pdu.SubmitSM, sm)
			took := time.Since(t0)
			if err != nil {
				fail.Add(1)
				var st uint32 = 0xFFFFFFFF
				if se, ok := err.(*session.StatusError); ok {
					st = se.Status
				}
				statusMu.Lock()
				statuses[st]++
				statusMu.Unlock()
				return
			}
			id := resp.Body.(*pdu.SMResp).MessageID
			if stat, found := early.LoadAndDelete(id); found {
				countReceipt(stat.(string))
			} else {
				sent.Store(id, struct{}{})
				if stat, found := early.LoadAndDelete(id); found && sent.CompareAndDelete(id, struct{}{}) {
					countReceipt(stat.(string))
				}
			}
			ok.Add(1)
			lat.record(took)
		}()
	}
	wg.Wait()
	sendEnd := time.Now()
	elapsed := sendEnd.Sub(start)

	rep := report{
		Started: start.UTC(), TargetTPS: *tps, DurationSeconds: elapsed.Seconds(),
		Accepted: ok.Load(), Failed: fail.Load(), AcceptedTPS: float64(ok.Load()) / elapsed.Seconds(),
		LatencyMs: map[string]float64{},
	}
	fmt.Printf("\nsubmits: %d ok, %d failed in %s -> %.0f accepted TPS\n",
		rep.Accepted, rep.Failed, elapsed.Round(time.Millisecond), rep.AcceptedTPS)
	if len(statuses) > 0 {
		rep.FailedByStatus = map[string]int{}
		keys := make([]uint32, 0, len(statuses))
		for k := range statuses {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			fmt.Printf("  status 0x%08X: %d\n", k, statuses[k])
			rep.FailedByStatus[fmt.Sprintf("0x%08X", k)] = statuses[k]
		}
	}
	all := lat.snapshot()
	p99 := all.quantile(0.99)
	if all.n > 0 {
		for _, q := range []struct {
			k string
			p float64
		}{{"p50", .50}, {"p95", .95}, {"p99", .99}, {"p999", .999}} {
			rep.LatencyMs[q.k] = ms(all.quantile(q.p))
		}
		mx := time.Duration(lat.max.Load()) * time.Microsecond
		rep.LatencyMs["max"] = ms(mx)
		fmt.Printf("submit_sm_resp latency: p50 %s  p95 %s  p99 %s  max %s\n",
			all.quantile(0.50), all.quantile(0.95), p99, mx.Round(time.Microsecond*100))
	}

	deadline := time.Now().Add(*drain)
	for time.Now().Before(deadline) && receipts.Load() < ok.Load() {
		time.Sleep(200 * time.Millisecond)
	}
	rep.Receipts, rep.Delivered = receipts.Load(), delivered.Load()
	if last := lastReceipt.Load(); last > 0 {
		rep.ReceiptLagSec = max(0, time.Unix(0, last).Sub(sendEnd).Seconds())
	}
	fmt.Printf("receipts: %d of %d (%d DELIVRD); last receipt %.1fs after sending stopped\n",
		rep.Receipts, rep.Accepted, rep.Delivered, rep.ReceiptLagSec)
	for i := range sessions {
		sessions[i].Load().Unbind(context.Background())
	}
	rep.Intervals, rep.Reconnects = intervals, reconnects.Load()

	// Gate.
	if rep.Receipts < rep.Accepted {
		rep.Failures = append(rep.Failures, fmt.Sprintf("%d receipts missing after %s drain", rep.Accepted-rep.Receipts, *drain))
	}
	if *minRatio > 0 && rep.AcceptedTPS < *minRatio*float64(*tps) {
		rep.Failures = append(rep.Failures, fmt.Sprintf("accepted %.0f TPS < %.0f%% of %d", rep.AcceptedTPS, *minRatio*100, *tps))
	}
	if *maxP99 > 0 && p99 > *maxP99 {
		rep.Failures = append(rep.Failures, fmt.Sprintf("p99 %s > %s", p99.Round(time.Millisecond), *maxP99))
	}
	for _, iv := range rep.Intervals {
		if *maxP99 > 0 && iv.P99Ms > ms(*maxP99) {
			rep.Failures = append(rep.Failures, fmt.Sprintf("interval ending %s: p99 %.1fms > %s", iv.End.Format(time.RFC3339), iv.P99Ms, *maxP99))
		}
		if *maxOutstanding > 0 && iv.Outstanding > *maxOutstanding {
			rep.Failures = append(rep.Failures, fmt.Sprintf("interval ending %s: %d messages waiting for receipts > %d", iv.End.Format(time.RFC3339), iv.Outstanding, *maxOutstanding))
		}
	}
	if *maxFail >= 0 && rep.Failed > *maxFail {
		rep.Failures = append(rep.Failures, fmt.Sprintf("%d submits refused > %d allowed", rep.Failed, *maxFail))
	}
	if *maxLag > 0 && rep.ReceiptLagSec > maxLag.Seconds() {
		rep.Failures = append(rep.Failures, fmt.Sprintf("receipt lag %.1fs > %s", rep.ReceiptLagSec, *maxLag))
	}
	rep.Pass = len(rep.Failures) == 0
	if *reportPath != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*reportPath, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "report:", err)
		}
	}
	if rep.Pass {
		fmt.Println("RESULT: PASS")
		return
	}
	fmt.Println("RESULT: FAIL")
	for _, f := range rep.Failures {
		fmt.Println("  -", f)
	}
	os.Exit(2)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
