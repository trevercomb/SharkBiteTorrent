//go:build windows || (linux && cgo) || (darwin && cgo)

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
// a site whose certificates don't check out. The window's connections then
// go to the address that was checked, not to whichever one the name gives
// next (a site's servers behind one name may not all send the same chain).
// Several servers behind that one address could still differ; that's the
// one case this can't see, since TLS 1.3 encrypts the certificates WebKit
// gets. Plain HTTP passes unchecked.

// How long verdicts are kept: whether a site's certificates check out (and
// the address that was checked); a TLS handshake that failed for another
// reason, briefly, so every connection to a site that won't handshake
// doesn't redo it. A connection that couldn't be made isn't kept.
const (
	tlsVetFor       = 10 * time.Minute
	tlsVetFailedFor = 30 * time.Second
)

type tlsVetter struct {
	dial  socks.Dialer
	roots *x509.CertPool // nil: the app's roots (tests set their own)

	mu sync.Mutex
	m  map[string]*tlsVerdict // by host:port
}

type tlsVerdict struct {
	done chan struct{} // closed once the fields below are set
	err  error
	addr string // the checked server's ip:port, if it checked out
	at   time.Time
	keep time.Duration
}

// open is the window proxy's socks.Server Open. A connection that starts
// with a TLS handshake goes to the server whose certificates were checked,
// and is refused if they don't check out; anything else goes to target as
// it is. Concurrent connections to one site share one check.
func (v *tlsVetter) open(ctx context.Context, target string, first []byte) (net.Conn, error) {
	if !tlsHello(first) {
		return v.dial(ctx, "tcp", target)
	}
	e, err := v.verdict(ctx, target)
	if err != nil {
		return nil, err
	}
	if e.err != nil {
		hostLog("refused (certificates):", target, e.err)
		return nil, e.err
	}
	up, err := v.dial(ctx, "tcp", e.addr)
	if err != nil {
		v.drop(target, e) // checked again with the next connection
		return nil, err
	}
	return up, nil
}

// verdict returns target's kept verdict, or checks it (once for everyone
// asking at the same time).
func (v *tlsVetter) verdict(ctx context.Context, target string) (*tlsVerdict, error) {
	v.mu.Lock()
	if v.m == nil {
		v.m = map[string]*tlsVerdict{}
	}
	e := v.m[target]
	if e != nil {
		select {
		case <-e.done:
			if time.Since(e.at) > e.keep {
				e = nil
			}
		default: // being checked: wait for it below
		}
	}
	if e == nil {
		e = &tlsVerdict{done: make(chan struct{})}
		v.m[target] = e
		v.mu.Unlock()
		addr, err := v.check(ctx, target)
		var cv *tls.CertificateVerificationError
		e.addr, e.err, e.at = addr, err, time.Now()
		switch {
		case err == nil || errors.As(err, &cv):
			e.keep = tlsVetFor
		case addr != "": // connected, but the handshake failed
			e.keep = tlsVetFailedFor
		}
		close(e.done)
		if e.keep == 0 {
			v.drop(target, e)
		}
	} else {
		v.mu.Unlock()
	}
	select {
	case <-e.done:
		return e, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// drop forgets target's verdict, if it's still e.
func (v *tlsVetter) drop(target string, e *tlsVerdict) {
	v.mu.Lock()
	if v.m[target] == e {
		delete(v.m, target)
	}
	v.mu.Unlock()
}

// check makes a TLS connection to target the way the window would and
// reports whether its certificates check out for target's host name (or
// IP address), and the address it reached ("" if it couldn't connect).
func (v *tlsVetter) check(ctx context.Context, target string) (string, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return "", err
	}
	raw, err := v.dial(ctx, "tcp", target)
	if err != nil {
		return "", err
	}
	defer raw.Close()
	addr := target // if the connection can't say where it went
	if ip, _, err := net.SplitHostPort(raw.RemoteAddr().String()); err == nil && net.ParseIP(ip) != nil {
		addr = net.JoinHostPort(ip, port)
	}
	c := tls.Client(raw, &tls.Config{ServerName: host, RootCAs: v.roots, NextProtos: []string{"h2", "http/1.1"}})
	return addr, c.HandshakeContext(ctx)
}

// tlsHello reports whether a connection's first bytes are a TLS handshake
// record (a ClientHello: content type 22, protocol version 3.x).
func tlsHello(b []byte) bool {
	return len(b) >= 3 && b[0] == 0x16 && b[1] == 0x03
}
