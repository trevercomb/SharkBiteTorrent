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
	"sync/atomic"
	"testing"
	"time"
)

// testChain makes a root, an intermediate and a certificate for 127.0.0.1
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
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
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
	full := tlsSite(t, [][]byte{leaf, inter}, key)
	partial := tlsSite(t, [][]byte{leaf}, key) // leaves out the intermediate

	var dials atomic.Int32
	d := &net.Dialer{}
	v := &tlsVetter{roots: roots, dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return d.DialContext(ctx, network, addr)
	}}
	hello := []byte{0x16, 0x03, 0x01, 0x02, 0x00}
	ctx := context.Background()

	if err := v.vet(ctx, full, hello); err != nil {
		t.Errorf("full chain refused: %v", err)
	}
	var cv *tls.CertificateVerificationError
	if err := v.vet(ctx, partial, hello); !errors.As(err, &cv) {
		t.Errorf("incomplete chain: got %v, want a certificate error", err)
	}
	if n := dials.Load(); n != 2 {
		t.Fatalf("dials: %d", n)
	}

	// Verdicts are kept: no new check.
	if v.vet(ctx, full, hello) != nil || v.vet(ctx, partial, hello) == nil || dials.Load() != 2 {
		t.Errorf("verdicts weren't kept (dials %d)", dials.Load())
	}
	// Plain HTTP isn't checked.
	if err := v.vet(ctx, partial, []byte("GET / HTTP/1.1\r\n")); err != nil || dials.Load() != 2 {
		t.Errorf("plain HTTP: %v (dials %d)", err, dials.Load())
	}

	// A connection that fails isn't a verdict: the next one checks again.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	gone := ln.Addr().String()
	ln.Close()
	before := dials.Load()
	v.vet(ctx, gone, hello)
	v.vet(ctx, gone, hello)
	if n := dials.Load() - before; n != 2 {
		t.Errorf("a failed connection was kept as a verdict (%d dials for 2 connections)", n)
	}
}
