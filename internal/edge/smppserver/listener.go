package smppserver

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/pires/go-proxyproto"
)

// ListenConfig says how the SMPP port is exposed.
type ListenConfig struct {
	Addr string
	// ProxyFrom lists the load balancers (addresses or CIDRs) that put the
	// client's address in a PROXY protocol header (HAProxy send-proxy-v2).
	// They must send it; anyone else sending one is refused. Without it,
	// behind a TCP load balancer every client would look like the balancer,
	// and per-address limits and IP allow lists would not work.
	ProxyFrom []string
	// CertFile and KeyFile turn on TLS on this port (SMPP over TLS).
	CertFile, KeyFile string
}

// Listen opens the SMPP listener.
func Listen(c ListenConfig) (net.Listener, error) {
	ln, err := net.Listen("tcp", c.Addr)
	if err != nil {
		return nil, err
	}
	if len(c.ProxyFrom) > 0 {
		policy, err := proxyPolicy(c.ProxyFrom)
		if err != nil {
			ln.Close()
			return nil, err
		}
		ln = &proxyproto.Listener{Listener: ln, ConnPolicy: policy, ReadHeaderTimeout: 5 * time.Second}
	}
	if c.CertFile != "" || c.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("smpp tls: %w", err)
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}
	return ln, nil
}

// proxyPolicy requires a PROXY header from the listed balancers and refuses
// one from anyone else (a client could otherwise claim any address).
func proxyPolicy(from []string) (proxyproto.ConnPolicyFunc, error) {
	var prefixes []netip.Prefix
	for _, f := range from {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, fmt.Errorf("SMPP_PROXY_PROTOCOL_FROM: %q: %w", f, err)
			}
			prefixes = append(prefixes, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("SMPP_PROXY_PROTOCOL_FROM: %q: %w", f, err)
		}
		prefixes = append(prefixes, p)
	}
	return func(o proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
		a := addrOf(o.Upstream)
		for _, p := range prefixes {
			if p.Contains(a) {
				return proxyproto.REQUIRE, nil
			}
		}
		return proxyproto.REJECT, nil
	}, nil
}
