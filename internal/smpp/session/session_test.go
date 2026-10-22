package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
)

func pair(t *testing.T, server, client Config) (*Session, *Session) {
	t.Helper()
	a, b := net.Pipe()
	srv := New(a, server)
	cli := New(b, client)
	srv.Start()
	cli.Start()
	t.Cleanup(func() { srv.Close(); cli.Close() })
	return srv, cli
}

func TestRequestResponseConcurrent(t *testing.T) {
	var handled atomic.Int64
	srvCfg := Config{Handler: func(s *Session, req *pdu.PDU) {
		sm := req.Body.(*pdu.SM)
		handled.Add(1)
		_ = s.Respond(req, pdu.StatusOK, &pdu.SMResp{MessageID: "id-" + sm.Dest})
	}}
	_, cli := pair(t, srvCfg, Config{Window: 5})

	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dest := fmt.Sprint(i)
			resp, err := cli.Send(context.Background(), pdu.SubmitSM, &pdu.SM{Dest: dest, ShortMessage: []byte("x")})
			if err != nil {
				errs <- err
				return
			}
			if got := resp.Body.(*pdu.SMResp).MessageID; got != "id-"+dest {
				errs <- fmt.Errorf("response mismatch: %s for %s", got, dest)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if handled.Load() != 200 {
		t.Fatalf("handled %d", handled.Load())
	}
}

func TestStatusErrorAndGenericNack(t *testing.T) {
	srvCfg := Config{Handler: func(s *Session, req *pdu.PDU) {
		_ = s.Respond(req, pdu.StatusThrottled, nil)
	}}
	_, cli := pair(t, srvCfg, Config{})

	_, err := cli.Send(context.Background(), pdu.SubmitSM, &pdu.SM{Dest: "1"})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != pdu.StatusThrottled {
		t.Fatalf("want throttled StatusError, got %v", err)
	}

	// Unknown command gets generic_nack with invalid command ID.
	_, err = cli.Send(context.Background(), 0x00000103, nil)
	if !errors.As(err, &se) || se.Status != pdu.StatusInvCmdID {
		t.Fatalf("want generic_nack, got %v", err)
	}
}

func TestEnquireLinkKeepsIdleSessionAlive(t *testing.T) {
	cfg := Config{EnquireInterval: 100 * time.Millisecond, IdleTimeout: time.Second}
	srv, cli := pair(t, cfg, cfg)
	time.Sleep(2 * time.Second) // well past the idle timeout: only enquire_link keeps it open
	select {
	case <-srv.Done():
		t.Fatalf("server closed: %v", srv.err)
	case <-cli.Done():
		t.Fatalf("client closed: %v", cli.err)
	default:
	}
}

func TestDeadPeerIsDetected(t *testing.T) {
	a, b := net.Pipe()
	s := New(a, Config{EnquireInterval: 20 * time.Millisecond, IdleTimeout: 100 * time.Millisecond, ResponseTimeout: 50 * time.Millisecond})
	s.Start()
	// The peer reads nothing and never answers.
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := b.Read(buf); err != nil {
				return
			}
		}
	}()
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("idle session was not closed")
	}
	b.Close()
}

func TestUnbindClosesBothSides(t *testing.T) {
	srv, cli := pair(t, Config{}, Config{})
	cli.Unbind(context.Background())
	select {
	case <-srv.Done():
	case <-time.After(time.Second):
		t.Fatal("server did not close after unbind")
	}
	if _, err := cli.Send(context.Background(), pdu.EnquireLink, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after close: %v", err)
	}
}

func TestResponseTimeout(t *testing.T) {
	srvCfg := Config{Handler: func(s *Session, req *pdu.PDU) {}} // never answers
	_, cli := pair(t, srvCfg, Config{ResponseTimeout: 50 * time.Millisecond})
	if _, err := cli.Send(context.Background(), pdu.SubmitSM, &pdu.SM{Dest: "1"}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
}

// A peer that streams requests and never reads the replies must not make
// the session start a goroutine per request (memory exhaustion before any
// bind): replies wait for in-flight slots and the read loop stalls.
func TestNonReadingPeerIsBounded(t *testing.T) {
	a, b := net.Pipe() // writes block until the other side reads
	s := New(a, Config{MaxInflight: 8, EnquireInterval: 200 * time.Millisecond, ResponseTimeout: 200 * time.Millisecond})
	s.Start()
	defer s.Close()
	base := runtime.NumGoroutine()
	frame := pdu.Encode(&pdu.PDU{CommandID: pdu.EnquireLink, Seq: 1}, pdu.V34)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := b.Write(frame); err != nil {
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	if n := runtime.NumGoroutine() - base; n > 20 {
		t.Fatalf("%d extra goroutines for a peer that does not read", n)
	}
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("session should close a peer that stopped reading")
	}
	close(stop)
	b.Close()
}

// A handler panic closes that session, not the process.
func TestHandlerPanicClosesSession(t *testing.T) {
	a, b := net.Pipe()
	s := New(a, Config{Handler: func(*Session, *pdu.PDU) { panic("boom") }})
	s.Start()
	go func() {
		_, _ = b.Write(pdu.Encode(&pdu.PDU{CommandID: pdu.SubmitSM, Seq: 1, Body: &pdu.SM{Dest: "1"}}, pdu.V34))
	}()
	go func() { _, _ = io.Copy(io.Discard, b) }()
	select {
	case <-s.Done():
		if s.Err() == nil || !strings.Contains(s.Err().Error(), "panic") {
			t.Fatalf("err %v", s.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session should close after a handler panic")
	}
}
