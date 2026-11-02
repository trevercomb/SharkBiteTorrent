package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync"
	"time"

	"sharkbite/internal/socks"
)

// The browser window's certificates are checked in its proxy too, before
// WebKit sees them. WebKit has the system check a site's certificates
// before the window's own offline check runs (window_darwin.m), and on
// macOS 27.2 that system check fetches a certificate the server left out
// (an intermediate) from its issuer, over the normal connection, outside
// the VPN: even when the window refuses the site without checking
// anything. So for each site, the proxy first makes a TLS connection of its
// own through the VPN and checks the chain with Go (the app's roots, see
// roots.go; Go never goes online), and refuses the window's connections to
// a site whose certificates don't check out. WebKit then never sees the
// incomplete chain, and a server that doesn't send its full chain fails, as
// it does for the app. Plain HTTP passes unchecked.

// tlsVetFor is how long a site's verdict is kept.
const tlsVetFor = 10 * time.Minute

type tlsVetter struct {
	dial  socks.Dialer
	roots *x509.CertPool // nil: the app's roots (tests set their own)

	mu sync.Mutex
	m  map[string]*tlsVerdict // by host:port
}

type tlsVerdict struct {
	done chan struct{} // closed once err and at are set
	err  error
	at   time.Time
}

// vet is a socks.Server's Vet: it lets a connection through unless it opens
// with a TLS handshake to a site whose certificates don't check out.
// Concurrent connections to one site share one check.
func (v *tlsVetter) vet(ctx context.Context, target string, first []byte) error {
	if !tlsHello(first) {
		return nil
	}
	v.mu.Lock()
	if v.m == nil {
		v.m = map[string]*tlsVerdict{}
	}
	e := v.m[target]
	if e != nil {
		select {
		case <-e.done:
			if time.Since(e.at) > tlsVetFor {
				e = nil
			}
		default: // being checked: wait for it below
		}
	}
	if e == nil {
		e = &tlsVerdict{done: make(chan struct{})}
		v.m[target] = e
		v.mu.Unlock()
		e.err = v.check(ctx, target)
		e.at = time.Now()
		close(e.done)
		// Only the certificates' verdict is kept: a dropped connection or a
		// timeout is tried again with the next connection.
		var cv *tls.CertificateVerificationError
		if e.err != nil && !errors.As(e.err, &cv) {
			v.mu.Lock()
			if v.m[target] == e {
				delete(v.m, target)
			}
			v.mu.Unlock()
		}
	} else {
		v.mu.Unlock()
	}
	select {
	case <-e.done:
		if e.err != nil {
			hostLog("refused (certificates):", target, e.err)
		}
		return e.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// check makes a TLS connection to target the way the window would and
// reports whether its certificates check out for target's host name (or
// IP address).
func (v *tlsVetter) check(ctx context.Context, target string) error {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		return err
	}
	raw, err := v.dial(ctx, "tcp", target)
	if err != nil {
		return err
	}
	defer raw.Close()
	c := tls.Client(raw, &tls.Config{ServerName: host, RootCAs: v.roots, NextProtos: []string{"h2", "http/1.1"}})
	return c.HandshakeContext(ctx)
}

// tlsHello reports whether a connection's first bytes are a TLS handshake
// record (a ClientHello: content type 22, protocol version 3.x).
func tlsHello(b []byte) bool {
	return len(b) >= 3 && b[0] == 0x16 && b[1] == 0x03
}
