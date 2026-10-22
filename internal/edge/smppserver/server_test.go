package smppserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pires/go-proxyproto"

	"github.com/jpamaze/amazeA2pSMS/internal/auth"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/session"
)

// fakeAuth knows one credential: demo/secret, allowed from 10.9.9.9 only
// when onlyFrom is set.
type fakeAuth struct{ onlyFrom bool }

func (f fakeAuth) SMPP(_ context.Context, systemID, password string, remote netip.Addr) (auth.SMPPCredential, error) {
	if systemID != "demo" || password != "secret" {
		return auth.SMPPCredential{}, auth.ErrInvalidCredentials
	}
	if f.onlyFrom && remote != netip.MustParseAddr("10.9.9.9") {
		return auth.SMPPCredential{}, auth.ErrIPNotAllowed
	}
	return auth.SMPPCredential{}, errors.New("test: bind succeeded past auth")
}

func start(t *testing.T, cfg Config, a Authenticator) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := New(cfg, a, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() { _ = srv.Serve(ctx, ln) }()
	return ln.Addr().String()
}

func bindStatus(t *testing.T, addr, user, pass string) uint32 {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	s := session.New(conn, session.Config{})
	s.Start()
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = s.Send(ctx, pdu.BindTransceiver, &pdu.Bind{SystemID: user, Password: pass, InterfaceVersion: pdu.V34})
	var se *session.StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	t.Fatalf("bind: %v", err)
	return 0
}

func TestRefusedBindsLookAlike(t *testing.T) {
	addr := start(t, Config{}, fakeAuth{onlyFrom: true})
	wrong := bindStatus(t, addr, "demo", "nope")
	rightButWrongPlace := bindStatus(t, addr, "demo", "secret")
	if wrong != pdu.StatusBindFail || rightButWrongPlace != pdu.StatusBindFail {
		t.Fatalf("a right password from a refused address must look like a wrong one: 0x%X vs 0x%X", wrong, rightButWrongPlace)
	}
}

func TestRepeatedBindFailuresBlockTheAddress(t *testing.T) {
	a := &countingAuth{}
	addr := start(t, Config{MaxBindFails: 3}, a)
	for range 3 {
		bindStatus(t, addr, "demo", "guess")
	}
	if got := bindStatus(t, addr, "demo", "secret"); got != pdu.StatusBindFail || a.calls != 3 {
		t.Fatalf("after 3 failures the address is refused without checking: status 0x%X, %d password checks", got, a.calls)
	}
}

type countingAuth struct{ calls int }

func (c *countingAuth) SMPP(ctx context.Context, systemID, password string, remote netip.Addr) (auth.SMPPCredential, error) {
	c.calls++
	return fakeAuth{}.SMPP(ctx, systemID, password, remote)
}

func TestConnectionLimitAndBindTimeout(t *testing.T) {
	addr := start(t, Config{MaxConnsPerIP: 2, BindTimeout: 300 * time.Millisecond}, fakeAuth{})
	var conns []net.Conn
	for range 3 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		time.Sleep(50 * time.Millisecond)
	}
	closedSoon := func(c net.Conn, within time.Duration) bool {
		_ = c.SetReadDeadline(time.Now().Add(within))
		_, err := c.Read(make([]byte, 1))
		var ne net.Error
		return err != nil && !(errors.As(err, &ne) && ne.Timeout())
	}
	if !closedSoon(conns[2], 200*time.Millisecond) {
		t.Fatal("the third connection from one address must be closed at once")
	}
	if closedSoon(conns[0], 100*time.Millisecond) {
		t.Fatal("connections within the limit stay open")
	}
	if !closedSoon(conns[0], time.Second) {
		t.Fatal("a connection that never binds is closed after the bind timeout")
	}
}

func TestProxyProtocolOnlyFromBalancers(t *testing.T) {
	policy, err := proxyPolicy([]string{"10.0.1.61", "10.0.2.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]proxyproto.Policy{
		"10.0.1.61:40000": proxyproto.REQUIRE, // a balancer must send the header
		"10.0.2.7:40000":  proxyproto.REQUIRE,
		"203.0.113.5:999": proxyproto.REJECT, // anyone else may not claim an address
	} {
		up, _ := net.ResolveTCPAddr("tcp", addr)
		if got, _ := policy(proxyproto.ConnPolicyOptions{Upstream: up}); got != want {
			t.Errorf("%s: policy %v, want %v", addr, got, want)
		}
	}
	if _, err := proxyPolicy([]string{"not-an-ip"}); err == nil {
		t.Fatal("bad address must be refused")
	}

	// Through the listener, the client's address comes from the header.
	ln, err := Listen(ListenConfig{Addr: "127.0.0.1:0", ProxyFrom: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan net.Addr, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Read(make([]byte, 1))
		got <- c.RemoteAddr()
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h := proxyproto.HeaderProxyFromAddrs(2, &net.TCPAddr{IP: net.ParseIP("198.51.100.20"), Port: 5555}, c.LocalAddr())
	if _, err := h.WriteTo(c); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte{0})
	select {
	case a := <-got:
		if addrOf(a) != netip.MustParseAddr("198.51.100.20") {
			t.Fatalf("remote %s", a)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no connection")
	}
}
