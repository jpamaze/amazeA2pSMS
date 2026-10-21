// Package smppserver is the customer-facing SMPP edge. It authenticates
// binds, accepts submit_sm through the ingest pipeline (answering only after
// the message is in Kafka) and pushes delivery receipts to receiver binds.
package smppserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/platform/metrics"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/jpamaze/amazeA2pSMS/internal/auth"
	"github.com/jpamaze/amazeA2pSMS/internal/dlt"
	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/firewall"
	"github.com/jpamaze/amazeA2pSMS/internal/ingest"
	"github.com/jpamaze/amazeA2pSMS/internal/msgstore"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/gsm7"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/session"
)

// Vendor-specific command_status values (SMPP reserves 0x400-0x4FF for them).
const (
	StatusInsufficientBalance uint32 = 0x00000401
	StatusNoPrice             uint32 = 0x00000402
	StatusAccountInactive     uint32 = 0x00000403 // account or its reseller suspended
	StatusResellerBilling     uint32 = 0x00000404 // reseller balance or rate missing
)

// DLTTags says which TLVs carry India DLT fields on customer submit_sm.
type DLTTags struct {
	PEID, TemplateID, TMChain uint16
}

// DefaultDLTTags are the tags most Indian aggregators use.
var DefaultDLTTags = DLTTags{PEID: 0x1400, TemplateID: 0x1401, TMChain: 0x1402}

// Config tunes the server.
type Config struct {
	SystemID        string // returned in bind_resp
	NodeID          string // unique per process; names this node's receipt consumer
	DLT             DLTTags
	EnquireInterval time.Duration
	Window          int // outstanding deliver_sm per receiver bind
}

// Authenticator is the subset of auth.Authenticator the server needs.
type Authenticator interface {
	SMPP(ctx context.Context, systemID, password string, remote netip.Addr) (auth.SMPPCredential, error)
}

// Acceptor is the subset of ingest.Pipeline the server needs.
type Acceptor interface {
	Accept(ctx context.Context, req ingest.Request) (ingest.Result, error)
}

// Server is the customer SMPP edge.
type Server struct {
	cfg    Config
	auth   Authenticator
	accept Acceptor
	store  *msgstore.Store
	log    *slog.Logger

	mu     sync.Mutex
	byAcct map[string][]*session.Session // receiver-capable sessions per account
	all    map[*session.Session]struct{}
	pumps  map[string]context.CancelFunc
	rr     map[string]int
	ctx    context.Context
}

type bindKind int

const (
	kindTX bindKind = iota
	kindRX
	kindTRX
)

type bindState struct {
	cred    auth.SMPPCredential
	kind    bindKind
	limiter *rate.Limiter
	lease   string // member in the cross-node bind registry
}

// bindLease is how long a bind slot survives without a refresh (a crashed
// node's binds free up after this).
const bindLease = 90 * time.Second

// New returns a Server.
func New(cfg Config, a Authenticator, acc Acceptor, store *msgstore.Store, log *slog.Logger) *Server {
	if cfg.SystemID == "" {
		cfg.SystemID = "AMAZE"
	}
	if cfg.DLT == (DLTTags{}) {
		cfg.DLT = DefaultDLTTags
	}
	if cfg.EnquireInterval == 0 {
		cfg.EnquireInterval = 30 * time.Second
	}
	if cfg.Window == 0 {
		cfg.Window = 10
	}
	return &Server{
		cfg: cfg, auth: a, accept: acc, store: store, log: log,
		byAcct: map[string][]*session.Session{},
		all:    map[*session.Session]struct{}{}, pumps: map[string]context.CancelFunc{}, rr: map[string]int{},
	}
}

// Serve accepts connections until ctx is cancelled, then unbinds every session.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	go func() { <-ctx.Done(); ln.Close() }()
	go s.refreshLeases(ctx)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				s.shutdown()
				return nil
			}
			s.log.Warn("accept", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		sess := session.New(conn, session.Config{
			Window:          s.cfg.Window,
			EnquireInterval: s.cfg.EnquireInterval,
			Handler:         s.handle,
			OnClose:         s.onClose,
		})
		s.mu.Lock()
		s.all[sess] = struct{}{}
		s.mu.Unlock()
		sess.Start()
	}
}

func (s *Server) shutdown() {
	s.mu.Lock()
	sessions := make([]*session.Session, 0, len(s.all))
	for sess := range s.all {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, sess := range sessions {
		wg.Add(1)
		go func() { defer wg.Done(); sess.Unbind(context.Background()) }()
	}
	wg.Wait()
}

func (s *Server) handle(sess *session.Session, req *pdu.PDU) {
	switch req.CommandID {
	case pdu.BindTransmitter, pdu.BindReceiver, pdu.BindTransceiver:
		s.bind(sess, req)
	case pdu.SubmitSM:
		s.submit(sess, req)
	default:
		_ = sess.Respond(req, pdu.StatusInvCmdID, nil)
	}
}

func (s *Server) bind(sess *session.Session, req *pdu.PDU) {
	respBody := &pdu.BindResp{SystemID: s.cfg.SystemID}
	if sess.Data() != nil {
		_ = sess.Respond(req, pdu.StatusAlyBnd, nil)
		return
	}
	b := req.Body.(*pdu.Bind)
	version := pdu.V34
	switch {
	case b.InterfaceVersion < pdu.V34:
		version = pdu.V33
	case b.InterfaceVersion >= pdu.V50:
		version = pdu.V50
	}
	sess.SetVersion(version)

	remote := netip.Addr{}
	if ap, err := netip.ParseAddrPort(sess.RemoteAddr().String()); err == nil {
		remote = ap.Addr()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cred, err := s.auth.SMPP(ctx, b.SystemID, b.Password, remote)
	if err != nil {
		status := pdu.StatusBindFail
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			status = pdu.StatusInvPaswd
		case errors.Is(err, auth.ErrIPNotAllowed), errors.Is(err, auth.ErrAccountInactive):
			status = pdu.StatusBindFail
		}
		s.log.Info("bind refused", "system_id", b.SystemID, "remote", remote.String(), "err", err)
		_ = sess.Respond(req, status, nil)
		time.AfterFunc(100*time.Millisecond, sess.Close)
		return
	}

	kind := kindTRX
	switch req.CommandID {
	case pdu.BindTransmitter:
		kind = kindTX
	case pdu.BindReceiver:
		kind = kindRX
	}

	// Max binds is enforced across every SMPP node through Redis.
	lease := s.cfg.NodeID + ":" + uuid.NewString()
	ok, err := s.store.AcquireBind(ctx, cred.SystemID, lease, cred.MaxBinds, bindLease)
	if err != nil || !ok {
		_ = sess.Respond(req, pdu.StatusBindFail, nil)
		time.AfterFunc(100*time.Millisecond, sess.Close)
		s.log.Info("bind refused: max binds", "system_id", cred.SystemID, "max", cred.MaxBinds, "err", err)
		return
	}
	tps := cred.TPS
	s.mu.Lock()
	sess.SetData(&bindState{cred: cred, kind: kind, lease: lease, limiter: rate.NewLimiter(rate.Limit(tps), max(1, tps/10))})
	var pumpCtx context.Context
	if kind != kindTX {
		s.byAcct[cred.AccountID] = append(s.byAcct[cred.AccountID], sess)
		if _, running := s.pumps[cred.AccountID]; !running && s.ctx != nil {
			var cancel context.CancelFunc
			pumpCtx, cancel = context.WithCancel(s.ctx)
			s.pumps[cred.AccountID] = cancel
		}
	}
	s.mu.Unlock()
	if pumpCtx != nil {
		go s.pump(pumpCtx, cred.AccountID)
	}

	if version >= pdu.V34 {
		respBody.TLVs.Set(pdu.TagSCInterfaceVersion, []byte{pdu.V34})
	}
	_ = sess.Respond(req, pdu.StatusOK, respBody)
	metrics.CustomerBinds.Inc()
	s.log.Info("bound", "system_id", cred.SystemID, "account", cred.AccountID, "kind", kind, "version", version, "remote", remote.String())
}

func (s *Server) onClose(sess *session.Session, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.all, sess)
	st, _ := sess.Data().(*bindState)
	if st == nil {
		return
	}
	metrics.CustomerBinds.Dec()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.store.ReleaseBind(ctx, st.cred.SystemID, st.lease)
	}()
	if st.kind != kindTX {
		list := s.byAcct[st.cred.AccountID]
		for i, x := range list {
			if x == sess {
				list = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(s.byAcct, st.cred.AccountID)
			if cancel, ok := s.pumps[st.cred.AccountID]; ok {
				cancel()
				delete(s.pumps, st.cred.AccountID)
			}
		} else {
			s.byAcct[st.cred.AccountID] = list
		}
	}
	s.log.Info("unbound", "system_id", st.cred.SystemID, "err", err)
}

func (s *Server) submit(sess *session.Session, req *pdu.PDU) {
	st, _ := sess.Data().(*bindState)
	if st == nil || st.kind == kindRX {
		_ = sess.Respond(req, pdu.StatusInvBnd, nil)
		return
	}
	if !st.limiter.Allow() {
		metrics.Refused.WithLabelValues("smpp", "throttled").Inc()
		_ = sess.Respond(req, pdu.StatusThrottled, nil)
		return
	}
	sm := req.Body.(*pdu.SM)
	text, udh, status := decodeText(sm)
	if status != pdu.StatusOK {
		_ = sess.Respond(req, status, nil)
		return
	}
	r := ingest.Request{
		AccountID:          st.cred.AccountID,
		Channel:            domain.ChannelSMPP,
		Sender:             sm.Source,
		Destination:        sm.Dest,
		Text:               text,
		UDH:                udh,
		RegisteredDelivery: sm.RegisteredDelivery&0x03 != 0,
	}
	r.DLT.PEID, _ = sm.TLVs.String(s.cfg.DLT.PEID)
	r.DLT.TemplateID, _ = sm.TLVs.String(s.cfg.DLT.TemplateID)
	r.DLT.TMChain, _ = sm.TLVs.String(s.cfg.DLT.TMChain)

	res, err := s.accept.Accept(context.Background(), r)
	if err != nil {
		_ = sess.Respond(req, statusFor(err), nil)
		if errors.Is(err, ingest.ErrUnavailable) {
			s.log.Warn("submit unavailable", "account", st.cred.AccountID, "err", err)
		}
		return
	}
	_ = sess.Respond(req, pdu.StatusOK, &pdu.SMResp{MessageID: res.MessageID})
}

// decodeText splits off a UDH and decodes the text by data_coding.
// data_coding 0 (SMSC default) is treated as Latin-1/ASCII, which is what
// most ESMEs actually send.
func decodeText(sm *pdu.SM) (string, []byte, uint32) {
	payload := sm.Payload()
	var udh []byte
	if sm.HasUDH() {
		if len(payload) == 0 || int(payload[0])+1 > len(payload) {
			return "", nil, pdu.StatusInvMsgLen
		}
		n := int(payload[0]) + 1
		udh = append([]byte(nil), payload[:n]...)
		payload = payload[n:]
	}
	switch sm.DataCoding {
	case 0x00, 0x01, 0x03:
		return gsm7.DecodeLatin1(payload), udh, pdu.StatusOK
	case 0x08:
		if len(payload)%2 != 0 {
			return "", nil, pdu.StatusInvMsgLen
		}
		return gsm7.DecodeUCS2(payload), udh, pdu.StatusOK
	}
	return "", nil, pdu.StatusInvDCS
}

func statusFor(err error) uint32 {
	switch {
	case errors.Is(err, ingest.ErrInvalidDestination):
		return pdu.StatusInvDstAdr
	case errors.Is(err, ingest.ErrInvalidSender):
		return pdu.StatusInvSrcAdr
	case errors.Is(err, ingest.ErrEmptyMessage), errors.Is(err, ingest.ErrTooLong):
		return pdu.StatusInvMsgLen
	case errors.Is(err, ingest.ErrInsufficient):
		return StatusInsufficientBalance
	case errors.Is(err, ingest.ErrNoPrice):
		return StatusNoPrice
	case errors.Is(err, ingest.ErrInactive):
		return StatusAccountInactive
	case errors.Is(err, ingest.ErrResellerBalance), errors.Is(err, ingest.ErrResellerNoPrice):
		return StatusResellerBilling
	case errors.Is(err, ingest.ErrUnavailable):
		return pdu.StatusMsgQFul // temporary: the customer should retry
	}
	if e, ok := dlt.AsError(err); ok {
		return e.SMPPStatus()
	}
	if e, ok := firewall.AsError(err); ok {
		return e.SMPPStatus()
	}
	return pdu.StatusSysErr
}

// refreshLeases keeps this node's bind slots alive in the registry.
func (s *Server) refreshLeases(ctx context.Context) {
	tick := time.NewTicker(bindLease / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		bySystem := map[string][]string{}
		s.mu.Lock()
		for sess := range s.all {
			if st, ok := sess.Data().(*bindState); ok {
				bySystem[st.cred.SystemID] = append(bySystem[st.cred.SystemID], st.lease)
			}
		}
		s.mu.Unlock()
		if len(bySystem) == 0 {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := s.store.RefreshBinds(rctx, bySystem, bindLease); err != nil {
			s.log.Warn("bind lease refresh", "err", err)
		}
		cancel()
	}
}

// receiverFor picks a receiver-capable session for the account, round-robin.
func (s *Server) receiverFor(accountID string) *session.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.byAcct[accountID]
	if len(list) == 0 {
		return nil
	}
	i := s.rr[accountID] % len(list)
	s.rr[accountID] = i + 1
	return list[i]
}

// pump delivers queued receipts for one account while it has receiver binds
// on this node. Receipts not acknowledged by the customer stay in the stream
// and are reclaimed after a minute by whichever node holds a bind.
func (s *Server) pump(ctx context.Context, accountID string) {
	consumer := s.cfg.NodeID
	lastClaim := time.Time{}
	for ctx.Err() == nil {
		var batch []msgstore.PendingReceipt
		var err error
		if time.Since(lastClaim) > 30*time.Second {
			lastClaim = time.Now()
			batch, err = s.store.ClaimStaleReceipts(ctx, accountID, consumer, time.Minute, 100)
		}
		if err == nil && len(batch) == 0 {
			batch, err = s.store.ReadReceipts(ctx, accountID, consumer, 200, time.Second)
		}
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("receipt read", "account", accountID, "err", err)
				time.Sleep(time.Second)
			}
			continue
		}
		s.deliver(ctx, accountID, batch)
	}
}

func (s *Server) deliver(ctx context.Context, accountID string, batch []msgstore.PendingReceipt) {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		acks []string
	)
	for _, r := range batch {
		sess := s.receiverFor(accountID)
		if sess == nil {
			break // no bind left; the rest stays pending
		}
		wg.Add(1)
		go func(r msgstore.PendingReceipt) {
			defer wg.Done()
			sm := pdu.ReceiptSM(pdu.Receipt{
				ID: r.MessageID, Sub: 1, Dlvrd: boolInt(r.Stat == "DELIVRD"),
				SubmitDate: r.Submitted, DoneDate: r.Done, Stat: r.Stat, Err: errCode(r.Err),
			}, r.Sender, r.Dest)
			sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if _, err := sess.Send(sctx, pdu.DeliverSM, sm); err != nil {
				return
			}
			mu.Lock()
			acks = append(acks, r.EntryID)
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	if len(acks) > 0 {
		metrics.CustomerReceipts.WithLabelValues("smpp").Add(float64(len(acks)))
		actx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.store.AckReceipts(actx, accountID, acks...); err != nil {
			s.log.Warn("receipt ack", "account", accountID, "err", err)
		}
	}
}

func errCode(e string) string {
	if e == "" {
		return "000"
	}
	if len(e) > 3 {
		return e[len(e)-3:]
	}
	for len(e) < 3 {
		e = "0" + e
	}
	return e
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
