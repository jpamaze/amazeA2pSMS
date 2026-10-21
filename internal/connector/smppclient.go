// Package connector sends routed messages to one vendor and journals the
// vendor's delivery receipts and MO messages to Kafka.
package connector

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"

	"golang.org/x/time/rate"

	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/session"
)

// TLVProfile says which TLV tags carry DLT fields for this vendor.
type TLVProfile struct {
	PEID       uint16
	TemplateID uint16
	TMChain    uint16
}

// BindConfig is one outbound SMPP bind (a vendor_binds row).
type BindConfig struct {
	ID         string
	Host       string
	Port       int
	TLS        bool
	SystemID   string
	Password   string
	SystemType string
	Version    byte   // pdu.V33, V34 or V50
	BindType   string // transmitter | receiver | transceiver
	Window     int
	TPS        int
	TLVs       TLVProfile
}

// DeliverFunc handles a deliver_sm from the vendor and returns the
// command_status to answer with. It must journal before returning OK.
type DeliverFunc func(ctx context.Context, sm *pdu.SM) uint32

// Bind keeps one SMPP bind up, reconnecting with backoff.
type Bind struct {
	cfg     BindConfig
	log     *slog.Logger
	deliver DeliverFunc
	limiter *rate.Limiter
	vendor  string

	mu   sync.RWMutex
	sess *session.Session
	up   atomic.Bool
	upCh chan struct{} // closed and replaced whenever the bind comes up
}

// NewBind returns a bind for vendor; call Run to connect.
func NewBind(vendor string, cfg BindConfig, deliver DeliverFunc, log *slog.Logger) *Bind {
	if cfg.Window <= 0 {
		cfg.Window = 10
	}
	if cfg.TPS <= 0 {
		cfg.TPS = 100
	}
	return &Bind{
		cfg: cfg, deliver: deliver, vendor: vendor,
		log:     log.With("bind", cfg.ID, "host", fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)),
		limiter: rate.NewLimiter(rate.Limit(cfg.TPS), max(1, cfg.TPS/20)),
		upCh:    make(chan struct{}),
	}
}

// CanSend reports whether this bind type may submit.
func (b *Bind) CanSend() bool { return b.cfg.BindType != "receiver" }

// Up reports whether the bind is currently bound.
func (b *Bind) Up() bool { return b.up.Load() }

// Run connects and rebinds until ctx is cancelled.
func (b *Bind) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		sess, err := b.connect(ctx)
		if err != nil {
			b.log.Warn("vendor bind failed", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		b.mu.Lock()
		b.sess = sess
		b.up.Store(true)
		metrics.VendorBindsUp.WithLabelValues(b.vendor).Inc()
		close(b.upCh)
		b.upCh = make(chan struct{})
		b.mu.Unlock()
		b.log.Info("vendor bound", "version", fmt.Sprintf("0x%02x", b.cfg.Version), "type", b.cfg.BindType)

		select {
		case <-ctx.Done():
			sess.Unbind(context.Background())
		case <-sess.Done():
		}
		b.mu.Lock()
		b.up.Store(false)
		b.sess = nil
		b.mu.Unlock()
		metrics.VendorBindsUp.WithLabelValues(b.vendor).Dec()
		if ctx.Err() == nil {
			b.log.Warn("vendor bind lost", "err", sess.Err())
		}
	}
}

func (b *Bind) connect(ctx context.Context) (*session.Session, error) {
	addr := net.JoinHostPort(b.cfg.Host, fmt.Sprint(b.cfg.Port))
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var conn net.Conn
	var err error
	if b.cfg.TLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: b.cfg.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	sess := session.New(conn, session.Config{
		Version: b.cfg.Version,
		Window:  b.cfg.Window,
		Handler: func(s *session.Session, req *pdu.PDU) {
			if req.CommandID != pdu.DeliverSM {
				_ = s.Respond(req, pdu.StatusInvCmdID, nil)
				return
			}
			dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			status := b.deliver(dctx, req.Body.(*pdu.SM))
			_ = s.Respond(req, status, &pdu.SMResp{})
		},
	})
	sess.Start()

	cmd := pdu.BindTransceiver
	switch b.cfg.BindType {
	case "transmitter":
		cmd = pdu.BindTransmitter
	case "receiver":
		cmd = pdu.BindReceiver
	}
	bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = sess.Send(bctx, cmd, &pdu.Bind{
		SystemID: b.cfg.SystemID, Password: b.cfg.Password, SystemType: b.cfg.SystemType,
		InterfaceVersion: b.cfg.Version,
	})
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("bind: %w", err)
	}
	return sess, nil
}

// errNotBound is returned when the bind went down between pick and send.
var errNotBound = errors.New("connector: bind not up")

// Submit sends one submit_sm, respecting the bind's TPS limit.
func (b *Bind) Submit(ctx context.Context, sm *pdu.SM) (string, error) {
	if err := b.limiter.Wait(ctx); err != nil {
		return "", err
	}
	b.mu.RLock()
	sess := b.sess
	b.mu.RUnlock()
	if sess == nil {
		return "", errNotBound
	}
	resp, err := sess.Send(ctx, pdu.SubmitSM, sm)
	if err != nil {
		return "", err
	}
	id := resp.Body.(*pdu.SMResp).MessageID
	if id == "" {
		return "", errors.New("connector: vendor returned empty message_id")
	}
	return id, nil
}

// Pool spreads submits over a vendor's sending binds.
type Pool struct {
	binds []*Bind
	next  atomic.Uint64
	// Down, when set, reports that failover has taken the vendor out of
	// routing; Pick then stops waiting for a bind so the message can move.
	Down func() bool
}

// Binds reports sending binds up and in total.
func (p *Pool) Binds() (up, total int) {
	for _, b := range p.binds {
		if b.CanSend() {
			total++
			if b.Up() {
				up++
			}
		}
	}
	return up, total
}

// NewPool returns a pool of binds.
func NewPool(binds []*Bind) *Pool { return &Pool{binds: binds} }

// Pick returns an up sending bind, waiting until one is up or ctx ends.
func (p *Pool) Pick(ctx context.Context) (*Bind, error) {
	for {
		n := len(p.binds)
		start := int(p.next.Add(1))
		var waits []<-chan struct{}
		for i := range n {
			b := p.binds[(start+i)%n]
			if !b.CanSend() {
				continue
			}
			if b.Up() {
				return b, nil
			}
			b.mu.RLock()
			waits = append(waits, b.upCh)
			b.mu.RUnlock()
		}
		if len(waits) == 0 {
			return nil, errors.New("connector: vendor has no sending binds")
		}
		if p.Down != nil && p.Down() {
			return nil, ErrVendorDown
		}
		// Wait for any bind to come up (poll the rest every 200ms).
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-waits[0]:
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Window is the total number of outstanding submits the pool can carry.
func (p *Pool) Window() int {
	n := 0
	for _, b := range p.binds {
		if b.CanSend() {
			n += b.cfg.Window
		}
	}
	return max(n, 1)
}
