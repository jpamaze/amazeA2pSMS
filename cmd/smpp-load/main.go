// Command smpp-load binds to the platform as a customer, sends messages at a
// fixed rate and reports throughput, latency and delivery receipts.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/session"
)

func main() {
	addr := flag.String("addr", "localhost:2775", "platform SMPP address")
	user := flag.String("user", "demo", "system_id")
	pass := flag.String("pass", "demo123", "password")
	binds := flag.Int("binds", 4, "transceiver binds")
	window := flag.Int("window", 100, "outstanding submits per bind")
	tps := flag.Int("tps", 1000, "target submits per second (total)")
	dur := flag.Duration("duration", 30*time.Second, "how long to send")
	drain := flag.Duration("drain", 30*time.Second, "how long to wait for receipts after sending")
	sender := flag.String("sender", "AMZTST", "sender ID")
	text := flag.String("text", "Your OTP is 482913. Do not share it with anyone. -Amaze", "message text")
	flag.Parse()

	var (
		ok, fail, receipts, delivered atomic.Int64
		statusMu                      sync.Mutex
		statuses                      = map[uint32]int{}
		latMu                         sync.Mutex
		lats                          []time.Duration
		sent                          sync.Map // message ID -> struct{}
		early                         sync.Map // receipts that beat their submit_sm_resp
	)
	countReceipt := func(stat string) {
		receipts.Add(1)
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

	var sessions []*session.Session
	for i := range *binds {
		conn, err := net.DialTimeout("tcp", *addr, 5*time.Second)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dial:", err)
			os.Exit(1)
		}
		s := session.New(conn, session.Config{Window: *window, Handler: handler, MaxInflight: 256})
		s.Start()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err = s.Send(ctx, pdu.BindTransceiver, &pdu.Bind{SystemID: *user, Password: *pass, InterfaceVersion: pdu.V34})
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "bind %d: %v\n", i, err)
			os.Exit(1)
		}
		sessions = append(sessions, s)
	}
	fmt.Printf("bound %d sessions; sending %d TPS for %s\n", len(sessions), *tps, *dur)

	lim := rate.NewLimiter(rate.Limit(*tps), max(1, *tps/50))
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	sem := make(chan struct{}, *binds**window)
	var n uint64
	for ctx.Err() == nil {
		if lim.Wait(ctx) != nil {
			break
		}
		sem <- struct{}{}
		n++
		s := sessions[n%uint64(len(sessions))]
		dest := fmt.Sprintf("9198%08d", n%100000000)
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
			lat := time.Since(t0)
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
			latMu.Lock()
			lats = append(lats, lat)
			latMu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	fmt.Printf("\nsubmits: %d ok, %d failed in %s -> %.0f accepted TPS\n",
		ok.Load(), fail.Load(), elapsed.Round(time.Millisecond), float64(ok.Load())/elapsed.Seconds())
	if len(statuses) > 0 {
		keys := make([]uint32, 0, len(statuses))
		for k := range statuses {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			fmt.Printf("  status 0x%08X: %d\n", k, statuses[k])
		}
	}
	if len(lats) > 0 {
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		pct := func(p float64) time.Duration { return lats[int(float64(len(lats)-1)*p)] }
		fmt.Printf("submit_sm_resp latency: p50 %s  p95 %s  p99 %s  max %s\n",
			pct(0.50).Round(time.Microsecond*100), pct(0.95).Round(time.Microsecond*100),
			pct(0.99).Round(time.Microsecond*100), lats[len(lats)-1].Round(time.Microsecond*100))
	}

	deadline := time.Now().Add(*drain)
	for time.Now().Before(deadline) && receipts.Load() < ok.Load() {
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("receipts: %d of %d (%d DELIVRD) after %s\n",
		receipts.Load(), ok.Load(), delivered.Load(), time.Since(start).Round(time.Millisecond))
	for _, s := range sessions {
		s.Unbind(context.Background())
	}
	if receipts.Load() < ok.Load() || fail.Load() > 0 {
		os.Exit(2)
	}
}
