// Command smpp-sim is a fake vendor SMSC for development and load tests. It
// accepts any bind, answers submit_sm with a message ID and, after a delay,
// sends a delivery receipt on a receiver-capable bind.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/time/rate"

	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/session"
)

func main() {
	addr := flag.String("addr", ":2776", "listen address")
	delay := flag.Duration("dlr-delay", 200*time.Millisecond, "delay before the delivery receipt")
	failRate := flag.Float64("fail", 0.02, "fraction of messages reported UNDELIV")
	tps := flag.Int("tps", 0, "throttle above this many submits per second per bind (0 = no limit)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	log.Info("smpp-sim listening", "addr", *addr, "dlr_delay", *delay, "fail", *failRate)

	sim := &simulator{delay: *delay, failRate: *failRate, tps: *tps, log: log, receivers: map[*session.Session]bool{}}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go sim.report(ctx)
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		var s *session.Session
		s = session.New(conn, session.Config{
			Window:  100,
			Handler: sim.handle,
			OnClose: func(s *session.Session, _ error) { sim.drop(s) },
		})
		s.Start()
	}
}

type simulator struct {
	delay    time.Duration
	failRate float64
	tps      int
	log      *slog.Logger
	seq      atomic.Uint64
	submits  atomic.Int64
	receipts atomic.Int64

	mu        sync.Mutex
	receivers map[*session.Session]bool
	rr        []*session.Session
	next      int
}

type bindData struct {
	limiter *rate.Limiter
}

func (m *simulator) handle(s *session.Session, req *pdu.PDU) {
	switch req.CommandID {
	case pdu.BindTransmitter, pdu.BindReceiver, pdu.BindTransceiver:
		b := req.Body.(*pdu.Bind)
		s.SetVersion(b.InterfaceVersion)
		var lim *rate.Limiter
		if m.tps > 0 {
			lim = rate.NewLimiter(rate.Limit(m.tps), m.tps/10+1)
		}
		s.SetData(&bindData{limiter: lim})
		if req.CommandID != pdu.BindTransmitter {
			m.mu.Lock()
			m.receivers[s] = true
			m.rr = append(m.rr, s)
			m.mu.Unlock()
		}
		_ = s.Respond(req, pdu.StatusOK, &pdu.BindResp{SystemID: "SIM"})
		m.log.Info("bound", "system_id", b.SystemID, "remote", s.RemoteAddr().String())
	case pdu.SubmitSM:
		bd, _ := s.Data().(*bindData)
		if bd == nil {
			_ = s.Respond(req, pdu.StatusInvBnd, nil)
			return
		}
		if bd.limiter != nil && !bd.limiter.Allow() {
			_ = s.Respond(req, pdu.StatusThrottled, nil)
			return
		}
		sm := req.Body.(*pdu.SM)
		id := fmt.Sprintf("%016X", m.seq.Add(1))
		_ = s.Respond(req, pdu.StatusOK, &pdu.SMResp{MessageID: id})
		m.submits.Add(1)
		if sm.RegisteredDelivery&0x03 != 0 {
			submitted := time.Now()
			time.AfterFunc(m.delay, func() { m.sendReceipt(id, sm, submitted) })
		}
	default:
		_ = s.Respond(req, pdu.StatusInvCmdID, nil)
	}
}

func (m *simulator) sendReceipt(id string, sm *pdu.SM, submitted time.Time) {
	stat, errc := "DELIVRD", "000"
	if rand.Float64() < m.failRate {
		stat, errc = "UNDELIV", "001"
	}
	r := pdu.Receipt{ID: id, Sub: 1, Dlvrd: 1, SubmitDate: submitted, DoneDate: time.Now(), Stat: stat, Err: errc}
	for attempt := 0; attempt < 5; attempt++ {
		s := m.receiver()
		if s == nil {
			time.Sleep(time.Second)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := s.Send(ctx, pdu.DeliverSM, pdu.ReceiptSM(r, sm.Source, sm.Dest))
		cancel()
		if err == nil {
			m.receipts.Add(1)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (m *simulator) receiver() *session.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.rr) == 0 {
		return nil
	}
	m.next = (m.next + 1) % len(m.rr)
	return m.rr[m.next]
}

func (m *simulator) drop(s *session.Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.receivers, s)
	for i, x := range m.rr {
		if x == s {
			m.rr = append(m.rr[:i], m.rr[i+1:]...)
			break
		}
	}
}

func (m *simulator) report(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var lastS, lastR int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s, r := m.submits.Load(), m.receipts.Load()
			if s != lastS || r != lastR {
				m.log.Info("sim stats", "submits", s, "submit_tps", (s-lastS)/5, "receipts", r, "receipt_tps", (r-lastR)/5)
			}
			lastS, lastR = s, r
		}
	}
}
