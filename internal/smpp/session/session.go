// Package session runs one SMPP connection: reading and writing PDUs,
// matching responses to requests within a window, answering enquire_link
// and unbind, and closing idle or broken links. Customer binds (server side)
// and vendor binds (client side) both use it.
package session

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
)

// Handler handles one inbound request (bind, submit_sm, deliver_sm). It must
// answer with s.Respond. Handlers run concurrently, up to Config.MaxInflight.
type Handler func(s *Session, req *pdu.PDU)

// Config tunes a session.
type Config struct {
	Version         byte          // interface version used for encoding (TLVs only for 3.4+)
	Window          int           // max requests we have outstanding to the peer
	MaxInflight     int           // max inbound requests being handled at once
	EnquireInterval time.Duration // send enquire_link after this much silence
	IdleTimeout     time.Duration // close after this much silence
	ResponseTimeout time.Duration // give up waiting for a response
	Handler         Handler
	OnClose         func(s *Session, err error)
}

func (c *Config) defaults() {
	if c.Version == 0 {
		c.Version = pdu.V34
	}
	if c.Window <= 0 {
		c.Window = 10
	}
	if c.MaxInflight <= 0 {
		c.MaxInflight = 64
	}
	if c.EnquireInterval <= 0 {
		c.EnquireInterval = 30 * time.Second
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 3 * c.EnquireInterval
	}
	if c.ResponseTimeout <= 0 {
		c.ResponseTimeout = 10 * time.Second
	}
}

var (
	ErrClosed  = errors.New("session: closed")
	ErrTimeout = errors.New("session: response timeout")
)

// StatusError is a non-zero command_status in a response.
type StatusError struct {
	CommandID uint32
	Status    uint32
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("smpp: command 0x%08x failed with status 0x%08x", e.CommandID, e.Status)
}

// Session is one live SMPP connection.
type Session struct {
	cfg     Config
	conn    net.Conn
	version atomic.Uint32
	seq     atomic.Uint32

	writeMu sync.Mutex
	w       *bufio.Writer

	pendMu  sync.Mutex
	pending map[uint32]chan *pdu.PDU

	window   chan struct{}
	inflight chan struct{}

	lastRecv  atomic.Int64
	closed    chan struct{}
	closeOnce sync.Once
	err       error

	// Data is free for the owner to attach state (for example the bound
	// account). Set it before handlers that read it can run.
	data atomic.Value
}

// New wraps conn. Call Start to begin reading.
func New(conn net.Conn, cfg Config) *Session {
	cfg.defaults()
	s := &Session{
		cfg:      cfg,
		conn:     conn,
		w:        bufio.NewWriterSize(conn, 16*1024),
		pending:  make(map[uint32]chan *pdu.PDU),
		window:   make(chan struct{}, cfg.Window),
		inflight: make(chan struct{}, cfg.MaxInflight),
		closed:   make(chan struct{}),
	}
	s.version.Store(uint32(cfg.Version))
	s.lastRecv.Store(time.Now().UnixNano())
	return s
}

// Start runs the read loop and keepalive in the background.
func (s *Session) Start() {
	go s.readLoop()
	go s.keepalive()
}

// SetVersion changes the interface version used for encoding (after bind).
func (s *Session) SetVersion(v byte) { s.version.Store(uint32(v)) }

// Version is the interface version in use.
func (s *Session) Version() byte { return byte(s.version.Load()) }

// SetData attaches owner state.
func (s *Session) SetData(v any) { s.data.Store(v) }

// Data returns owner state, or nil.
func (s *Session) Data() any { return s.data.Load() }

// RemoteAddr is the peer address.
func (s *Session) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.closed }

// Err is why the session ended (nil for a clean unbind).
func (s *Session) Err() error {
	<-s.closed
	return s.err
}

// Close ends the session.
func (s *Session) Close() { s.closeWith(nil) }

func (s *Session) closeWith(err error) {
	s.closeOnce.Do(func() {
		s.err = err
		close(s.closed)
		s.conn.Close()
		// Pending Sends see s.closed; their channels are never closed, so the
		// read loop can still deliver into them without racing.
		if s.cfg.OnClose != nil {
			go s.cfg.OnClose(s, err)
		}
	})
}

func (s *Session) nextSeq() uint32 {
	for {
		v := s.seq.Add(1)
		if v != 0 && v <= 0x7FFFFFFF {
			return v
		}
		s.seq.CompareAndSwap(v, 0)
	}
}

func (s *Session) write(p *pdu.PDU) error {
	b := pdu.Encode(p, s.Version())
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	select {
	case <-s.closed:
		return ErrClosed
	default:
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.cfg.ResponseTimeout))
	if _, err := s.w.Write(b); err != nil {
		go s.closeWith(err)
		return err
	}
	if err := s.w.Flush(); err != nil {
		go s.closeWith(err)
		return err
	}
	return nil
}

// Send sends a request and waits for its response. It blocks while the
// window is full. A response with a non-zero status returns *StatusError
// together with the response.
func (s *Session) Send(ctx context.Context, commandID uint32, body pdu.Body) (*pdu.PDU, error) {
	select {
	case s.window <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closed:
		return nil, ErrClosed
	}
	defer func() { <-s.window }()

	seq := s.nextSeq()
	ch := make(chan *pdu.PDU, 1)
	s.pendMu.Lock()
	select {
	case <-s.closed:
		s.pendMu.Unlock()
		return nil, ErrClosed
	default:
	}
	s.pending[seq] = ch
	s.pendMu.Unlock()
	defer func() {
		s.pendMu.Lock()
		delete(s.pending, seq)
		s.pendMu.Unlock()
	}()

	if err := s.write(&pdu.PDU{CommandID: commandID, Seq: seq, Body: body}); err != nil {
		return nil, err
	}

	timer := time.NewTimer(s.cfg.ResponseTimeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		if resp.CommandID == pdu.GenericNack || resp.Status != pdu.StatusOK {
			return resp, &StatusError{CommandID: commandID, Status: resp.Status}
		}
		return resp, nil
	case <-s.closed:
		// A response may have arrived just before the close.
		select {
		case resp := <-ch:
			if resp.CommandID == pdu.GenericNack || resp.Status != pdu.StatusOK {
				return resp, &StatusError{CommandID: commandID, Status: resp.Status}
			}
			return resp, nil
		default:
			return nil, ErrClosed
		}
	case <-timer.C:
		return nil, ErrTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Respond answers req with status and an optional body.
func (s *Session) Respond(req *pdu.PDU, status uint32, body pdu.Body) error {
	return s.write(&pdu.PDU{CommandID: req.CommandID | 0x80000000, Status: status, Seq: req.Seq, Body: body})
}

// Unbind sends unbind, waits briefly for the response and closes.
func (s *Session) Unbind(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _ = s.Send(ctx, pdu.Unbind, nil)
	s.Close()
}

func (s *Session) readLoop() {
	r := bufio.NewReaderSize(s.conn, 16*1024)
	for {
		p, err := pdu.Read(r, s.Version())
		if err != nil && p == nil {
			s.closeWith(err)
			return
		}
		s.lastRecv.Store(time.Now().UnixNano())
		if err != nil { // body malformed, framing intact
			if !pdu.IsResponse(p.CommandID) {
				nack := &pdu.PDU{CommandID: pdu.GenericNack, Status: pdu.StatusInvMsgLen, Seq: p.Seq}
				if !s.async(func() { _ = s.write(nack) }) {
					return
				}
			}
			continue
		}
		if pdu.IsResponse(p.CommandID) {
			s.pendMu.Lock()
			ch, ok := s.pending[p.Seq]
			s.pendMu.Unlock()
			if ok {
				select {
				case ch <- p:
				default:
				}
			}
			continue
		}
		// The read loop never writes: if both peers blocked writing from their
		// read loops with full buffers, neither would read again (deadlock).
		// Every reply and handler runs under the same in-flight limit, so a
		// peer that sends without reading the replies stalls this loop (and
		// is closed by the idle check) instead of piling up goroutines.
		var ok bool
		switch {
		case p.CommandID == pdu.EnquireLink:
			ok = s.async(func() { _ = s.Respond(p, pdu.StatusOK, nil) })
		case p.CommandID == pdu.Unbind:
			s.async(func() {
				_ = s.Respond(p, pdu.StatusOK, nil)
				s.closeWith(nil)
			})
			return
		case !pdu.Known(p.CommandID) || s.cfg.Handler == nil:
			nack := &pdu.PDU{CommandID: pdu.GenericNack, Status: pdu.StatusInvCmdID, Seq: p.Seq}
			ok = s.async(func() { _ = s.write(nack) })
		default:
			ok = s.async(func() { s.cfg.Handler(s, p) })
		}
		if !ok {
			return
		}
	}
}

// async runs fn in its own goroutine once an in-flight slot is free. It
// reports false when the session closed while waiting. A panic in fn (a
// handler bug, or input nothing anticipated) closes this session only.
func (s *Session) async(fn func()) bool {
	select {
	case s.inflight <- struct{}{}:
	case <-s.closed:
		return false
	}
	go func() {
		defer func() { <-s.inflight }()
		defer func() {
			if r := recover(); r != nil {
				s.closeWith(fmt.Errorf("session: handler panic: %v", r))
			}
		}()
		fn()
	}()
	return true
}

func (s *Session) keepalive() {
	tick := time.NewTicker(s.cfg.EnquireInterval / 2)
	defer tick.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-tick.C:
			idle := time.Since(time.Unix(0, s.lastRecv.Load()))
			if idle > s.cfg.IdleTimeout {
				s.closeWith(fmt.Errorf("session: idle for %s", idle.Round(time.Second)))
				return
			}
			if idle > s.cfg.EnquireInterval {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ResponseTimeout)
					defer cancel()
					_, _ = s.Send(ctx, pdu.EnquireLink, nil)
				}()
			}
		}
	}
}
