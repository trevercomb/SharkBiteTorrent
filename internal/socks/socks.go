// Package socks is a minimal SOCKS5 server (RFC 1928, CONNECT only, with
// username/password auth from RFC 1929). The app runs it on 127.0.0.1 so
// Prowlarr or Jackett can send their traffic out through the VPN tunnel.
package socks

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Dialer opens outbound connections; the app passes the tunnel's dialer,
// which also resolves host names through the VPN's DNS.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

type Server struct {
	Username, Password string
	// NoAuth accepts clients without a password, for Chromium (WebView2),
	// which can't log in to a SOCKS proxy. Only for a short-lived listener.
	NoAuth bool
	Dial   Dialer

	// Requests counts CONNECT requests from clients that logged in, so the
	// app can confirm a search server really sends its traffic through here.
	Requests atomic.Int64
	// Sent counts bytes clients sent through here, in reads big enough to
	// carry a request (minRequest): a search server reuses connections it
	// already has open, so a search may make no new request, while closing
	// or keeping alive an idle one sends only a few bytes, which mustn't
	// count as a search having gone through.
	Sent atomic.Int64

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
}

// Serve accepts connections until Close. It returns nil after Close.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	if s.conns == nil {
		s.conns = map[net.Conn]struct{}{}
	}
	s.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(c)
	}
}

// Close stops listening and drops every open connection.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		s.ln.Close()
	}
	for c := range s.conns {
		c.Close()
	}
}

func (s *Server) track(c net.Conn, on bool) {
	s.mu.Lock()
	if on {
		s.conns[c] = struct{}{}
	} else {
		delete(s.conns, c)
	}
	s.mu.Unlock()
}

// Reply codes.
const (
	repOK          = 0x00
	repUnreachable = 0x04
	repCmdNotSup   = 0x07
	repAddrNotSup  = 0x08
)

func (s *Server) handle(c net.Conn) {
	s.track(c, true)
	defer s.track(c, false)
	defer c.Close()

	c.SetDeadline(time.Now().Add(30 * time.Second))
	target, err := s.negotiate(c)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	up, err := s.Dial(ctx, "tcp", target)
	cancel()
	if err != nil {
		reply(c, repUnreachable)
		return
	}
	defer up.Close()
	s.track(up, true)
	defer s.track(up, false)
	if reply(c, repOK) != nil {
		return
	}
	c.SetDeadline(time.Time{})

	done := make(chan struct{}, 2)
	pipe := func(dst net.Conn, src io.Reader) {
		io.Copy(dst, src)
		if tc, ok := dst.(interface{ CloseWrite() error }); ok {
			tc.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go pipe(up, countingReader{c, &s.Sent})
	go pipe(c, up)
	<-done
	<-done
}

// countingReader adds what it reads to n as it goes (io.Copy only reports
// the total once the connection ends), counting only request-sized reads.
type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

// minRequest is the smallest read Sent counts. A TLS close_notify alert is
// 31 bytes and a TCP keep-alive carries none; an HTTP request is more than
// that even in plain HTTP (a bare GET with Go's headers is about 95), and a
// few hundred inside TLS.
const minRequest = 64

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n >= minRequest {
		c.n.Add(int64(n))
	}
	return n, err
}

// negotiate runs the greeting, auth and request, and returns host:port.
func (s *Server) negotiate(c net.Conn) (string, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil || hdr[0] != 5 {
		return "", errors.New("not SOCKS5")
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", err
	}
	hasUserPass, hasNone := false, false
	for _, m := range methods {
		hasUserPass = hasUserPass || m == 0x02
		hasNone = hasNone || m == 0x00
	}
	switch {
	case s.NoAuth && hasNone:
		c.Write([]byte{5, 0x00})
		s.Requests.Add(1)
		return readRequest(c)
	case !hasUserPass:
		c.Write([]byte{5, 0xff}) // no acceptable method: we always require a password
		return "", errors.New("client doesn't offer username/password")
	}
	c.Write([]byte{5, 0x02})

	// RFC 1929: ver(1) ulen(1) user plen(1) pass
	var v [2]byte
	if _, err := io.ReadFull(c, v[:]); err != nil || v[0] != 1 {
		return "", errors.New("bad auth")
	}
	user := make([]byte, v[1])
	if _, err := io.ReadFull(c, user); err != nil {
		return "", err
	}
	if _, err := io.ReadFull(c, v[:1]); err != nil {
		return "", err
	}
	pass := make([]byte, v[0])
	if _, err := io.ReadFull(c, pass); err != nil {
		return "", err
	}
	okUser := subtle.ConstantTimeCompare(user, []byte(s.Username)) == 1
	okPass := subtle.ConstantTimeCompare(pass, []byte(s.Password)) == 1
	if !okUser || !okPass {
		c.Write([]byte{1, 1})
		return "", errors.New("wrong credentials")
	}
	c.Write([]byte{1, 0})
	s.Requests.Add(1)
	return readRequest(c)
}

func readRequest(c net.Conn) (string, error) {
	// Request: ver cmd rsv atyp addr port
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil || req[0] != 5 {
		return "", errors.New("bad request")
	}
	if req[1] != 0x01 {
		reply(c, repCmdNotSup)
		return "", errors.New("only CONNECT is supported")
	}
	var host string
	switch req[3] {
	case 0x01:
		var ip [4]byte
		if _, err := io.ReadFull(c, ip[:]); err != nil {
			return "", err
		}
		host = net.IP(ip[:]).String()
	case 0x04:
		var ip [16]byte
		if _, err := io.ReadFull(c, ip[:]); err != nil {
			return "", err
		}
		host = net.IP(ip[:]).String()
	case 0x03:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return "", err
		}
		name := make([]byte, n[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return "", err
		}
		host = string(name)
	default:
		reply(c, repAddrNotSup)
		return "", errors.New("bad address type")
	}
	var port [2]byte
	if _, err := io.ReadFull(c, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

func reply(c net.Conn, code byte) error {
	// The bound address isn't meaningful to clients here; send 0.0.0.0:0.
	_, err := c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}
