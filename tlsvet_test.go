package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

// testChain makes a root, an intermediate and a certificate for localhost
// issued by the intermediate.
func testChain(t *testing.T) (roots *x509.CertPool, leaf, inter []byte, key *ecdsa.PrivateKey) {
	t.Helper()
	newKey := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	now := time.Now()
	issue := func(tmpl, parent *x509.Certificate, pub, priv any) ([]byte, *x509.Certificate) {
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, priv)
		if err != nil {
			t.Fatal(err)
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return der, c
	}
	ca := func(n int64, name string) *x509.Certificate {
		return &x509.Certificate{SerialNumber: big.NewInt(n), Subject: pkix.Name{CommonName: name},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
			KeyUsage: x509.KeyUsageCertSign}
	}
	rootKey, interKey := newKey(), newKey()
	key = newKey()
	_, root := issue(ca(1, "test root"), ca(1, "test root"), &rootKey.PublicKey, rootKey)
	inter, interCert := issue(ca(2, "test intermediate"), root, &interKey.PublicKey, rootKey)
	leaf, _ = issue(&x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, interCert, &key.PublicKey, interKey)
	roots = x509.NewCertPool()
	roots.AddCert(root)
	return roots, leaf, inter, key
}

// tlsSite serves TLS handshakes with the given chain and returns its address.
func tlsSite(t *testing.T, chain [][]byte, key *ecdsa.PrivateKey) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: chain, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.(*tls.Conn).Handshake()
			}()
		}
	}()
	return ln.Addr().String()
}

func TestTLSVetter(t *testing.T) {
	roots, leaf, inter, key := testChain(t)
	port := func(addr string) string { _, p, _ := net.SplitHostPort(addr); return p }
	full := "localhost:" + port(tlsSite(t, [][]byte{leaf, inter}, key))
	partial := "localhost:" + port(tlsSite(t, [][]byte{leaf}, key)) // leaves out the intermediate

	var mu sync.Mutex
	var dialed []string
	d := &net.Dialer{}
	v := &tlsVetter{roots: roots, dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		return d.DialContext(ctx, network, addr)
	}}
	since := func() func() []string {
		mu.Lock()
		n := len(dialed)
		mu.Unlock()
		return func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), dialed[n:]...)
		}
	}
	hello := []byte{0x16, 0x03, 0x01, 0x02, 0x00}
	ctx := context.Background()
	open := func(target string, first []byte) error {
		c, err := v.open(ctx, target, first)
		if c != nil {
			c.Close()
		}
		return err
	}

	// A full chain: checked through the name, then connected to the address
	// that was checked, not to the name again.
	got := since()
	if err := open(full, hello); err != nil {
		t.Fatalf("full chain refused: %v", err)
	}
	if d := got(); len(d) != 2 || d[0] != full || d[1] != "127.0.0.1:"+port(full) {
		t.Errorf("full chain dialed %q, want the name to check, then its checked address", d)
	}
	// Kept: no new check, and the checked address again.
	got = since()
	if err := open(full, hello); err != nil || len(got()) != 1 || got()[0] != "127.0.0.1:"+port(full) {
		t.Errorf("kept verdict: %v, dialed %q", err, got())
	}

	// An incomplete chain is refused, without connecting for the window; kept.
	got = since()
	var cv *tls.CertificateVerificationError
	if err := open(partial, hello); !errors.As(err, &cv) {
		t.Errorf("incomplete chain: got %v, want a certificate error", err)
	}
	if err := open(partial, hello); err == nil || len(got()) != 1 {
		t.Errorf("incomplete chain again: %v, dialed %q (want only the first check)", err, got())
	}

	// Plain HTTP isn't checked: straight to the name.
	got = since()
	if err := open(partial, []byte("GET / HTTP/1.1\r\n")); err != nil || len(got()) != 1 || got()[0] != partial {
		t.Errorf("plain HTTP: %v, dialed %q", err, got())
	}

	// A server that won't do TLS: refused, and kept briefly.
	notTLS, _ := net.Listen("tcp", "127.0.0.1:0")
	defer notTLS.Close()
	go func() {
		for {
			c, err := notTLS.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	got = since()
	if open(notTLS.Addr().String(), hello) == nil || open(notTLS.Addr().String(), hello) == nil || len(got()) != 1 {
		t.Errorf("no TLS: dialed %q, want one check kept for both", got())
	}

	// A connection that can't be made isn't kept: the next one checks again.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	gone := ln.Addr().String()
	ln.Close()
	got = since()
	open(gone, hello)
	open(gone, hello)
	if len(got()) != 2 {
		t.Errorf("a failed connection was kept as a verdict (dialed %q)", got())
	}

	// The checked server stops answering: its verdict is dropped, so the
	// next connection checks the name again.
	site, _ := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf, inter}, PrivateKey: key}}})
	go func() {
		for {
			c, err := site.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); c.(*tls.Conn).Handshake() }()
		}
	}()
	moving := "localhost:" + port(site.Addr().String())
	if err := open(moving, hello); err != nil {
		t.Fatal(err)
	}
	site.Close()
	got = since()
	open(moving, hello) // the checked address is gone
	open(moving, hello) // so this one checks the name again
	if d := got(); len(d) != 2 || d[0] != "127.0.0.1:"+port(moving) || d[1] != moving {
		t.Errorf("after the checked server went away, dialed %q", d)
	}
}
