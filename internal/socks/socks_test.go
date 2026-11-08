package socks

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxy(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer site.Close()

	var dialed []string
	d := &net.Dialer{}
	s := &Server{Username: "u", Password: "secret", Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return d.DialContext(ctx, network, addr)
	}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	defer s.Close()

	get := func(user, pass string) (string, error) {
		proxy := &url.URL{Scheme: "socks5", Host: ln.Addr().String(), User: url.UserPassword(user, pass)}
		hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
		resp, err := hc.Get(site.URL)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), nil
	}
	if body, err := get("u", "secret"); err != nil || body != "hello" {
		t.Fatalf("through proxy: %q %v", body, err)
	}
	if s.Requests.Load() != 1 {
		t.Errorf("requests counted: %d", s.Requests.Load())
	}
	if len(dialed) != 1 || dialed[0] != site.Listener.Addr().String() {
		t.Errorf("dialed %v", dialed)
	}
	if _, err := get("u", "wrong"); err == nil {
		t.Error("wrong password was accepted")
	}
	if len(dialed) != 1 || s.Requests.Load() != 1 {
		t.Error("dialed out for (or counted) an unauthenticated client")
	}
}

// A client that keeps its connection open sends its next request without a
// new CONNECT; Sent must still show it, or the app's proxy check reports a
// search server that is using the proxy as not using it.
func TestSentCountsReusedConnections(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer site.Close()
	d := &net.Dialer{}
	s := &Server{Username: "u", Password: "secret", Dial: d.DialContext}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	defer s.Close()

	proxy := &url.URL{Scheme: "socks5", Host: ln.Addr().String(), User: url.UserPassword("u", "secret")}
	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
	get := func() {
		resp, err := hc.Get(site.URL)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	get()
	reqs, sent := s.Requests.Load(), s.Sent.Load()
	if sent == 0 {
		t.Fatal("the first request's bytes weren't counted")
	}
	get()
	if s.Requests.Load() != reqs {
		t.Fatalf("expected the second request to reuse the connection (requests %d → %d)", reqs, s.Requests.Load())
	}
	if s.Sent.Load() <= sent {
		t.Error("a request over a reused connection wasn't counted in Sent")
	}
}

// Bytes that can't be a request (a TLS close or keep-alive on an idle
// connection) don't count as traffic through the proxy.
func TestSentIgnoresTinyReads(t *testing.T) {
	var n atomic.Int64
	small := countingReader{strings.NewReader(strings.Repeat("x", minRequest-1)), &n}
	io.ReadAll(small)
	if n.Load() != 0 {
		t.Errorf("a %d-byte read was counted", minRequest-1)
	}
	big := countingReader{strings.NewReader(strings.Repeat("x", 300)), &n}
	io.ReadAll(big)
	if n.Load() != 300 {
		t.Errorf("a request-sized read counted %d bytes", n.Load())
	}
}

// Open sees the client's first bytes and opens the connection: allowed,
// they reach the site intact; refused, nothing connects to the site at all.
func TestOpen(t *testing.T) {
	got := make(chan []byte, 4)
	site, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer site.Close()
	go func() {
		for {
			c, err := site.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				got <- b
			}()
		}
	}()

	var mu sync.Mutex
	var opened []string
	var refuse atomic.Bool
	d := &net.Dialer{}
	s := &Server{NoAuth: true, Open: func(ctx context.Context, target string, first []byte) (net.Conn, error) {
		mu.Lock()
		opened = append(opened, target+" "+string(first))
		mu.Unlock()
		if refuse.Load() {
			return nil, io.ErrUnexpectedEOF
		}
		return d.DialContext(ctx, "tcp", target)
	}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	defer s.Close()

	send := func(msg string) {
		c, err := connectVia(ln.Addr().String(), site.Addr().(*net.TCPAddr))
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(c, msg)
		c.(*net.TCPConn).CloseWrite()
		io.ReadAll(c)
		c.Close()
	}

	send("hello, site")
	if b := <-got; string(b) != "hello, site" {
		t.Errorf("allowed: the site got %q", b)
	}
	refuse.Store(true)
	send("refused")
	select {
	case b := <-got:
		t.Errorf("refused: the site got a connection (%q)", b)
	case <-time.After(200 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	if len(opened) != 2 || opened[0] != site.Addr().String()+" hello, site" {
		t.Errorf("opened %q", opened)
	}
}

// connectVia opens a connection to an IPv4 address through a SOCKS5 server
// that needs no password.
func connectVia(proxy string, to *net.TCPAddr) (net.Conn, error) {
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		return nil, err
	}
	resp := make([]byte, 10)
	req := append([]byte{5, 1, 0, 1}, to.IP.To4()...)
	req = append(req, byte(to.Port>>8), byte(to.Port))
	if _, err = c.Write([]byte{5, 1, 0}); err == nil {
		if _, err = io.ReadFull(c, resp[:2]); err == nil {
			if _, err = c.Write(req); err == nil {
				_, err = io.ReadFull(c, resp)
			}
		}
	}
	if err == nil && resp[1] != 0 {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
