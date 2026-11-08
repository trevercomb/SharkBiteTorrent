package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"
)

// directConns: connections made without the VPN ("No VPN" mode). They're
// closed whenever the VPN mode changes (see disconnect), so none carries on
// over the normal connection once a VPN is chosen: a direct download, or a
// page the browser window keeps a connection open to. What used them
// connects again, through the VPN this time (or waits for it).
type directConns struct {
	mu    sync.Mutex
	gen   uint64 // counts closeAll calls: a dial that spans one is refused
	open  bool   // No VPN is running: set once it is, cleared by closeAll
	conns map[*directConn]struct{}
}

var errModeChanged = errors.New("the VPN setting changed while connecting")

// dial connects directly. guardLocal refuses this computer and the home
// network, checked on the address actually dialed (names that resolve
// there included).
func (s *directConns) dial(ctx context.Context, network, addr string, guardLocal bool) (net.Conn, error) {
	s.mu.Lock()
	gen, open := s.gen, s.open
	s.mu.Unlock()
	// A caller that decided on a direct connection just before the mode
	// started changing gets nothing, rather than a connection that would
	// outlive the change.
	if !open {
		return nil, errModeChanged
	}
	d := net.Dialer{Timeout: 30 * time.Second, Resolver: &net.Resolver{}}
	if guardLocal {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			if h, _, err := net.SplitHostPort(address); err != nil || localHost(h) {
				return fmt.Errorf("%s is on this computer or the home network", address)
			}
			return nil
		}
	}
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen || !s.open {
		c.Close()
		return nil, errModeChanged
	}
	dc := &directConn{Conn: c, s: s}
	if s.conns == nil {
		s.conns = map[*directConn]struct{}{}
	}
	s.conns[dc] = struct{}{}
	return dc, nil
}

// allow lets direct connections be made: "No VPN" is up.
func (s *directConns) allow() {
	s.mu.Lock()
	s.open = true
	s.mu.Unlock()
}

// closeAll closes every direct connection, and any being made now, and
// refuses new ones until allow.
func (s *directConns) closeAll() {
	s.mu.Lock()
	s.gen++
	s.open = false
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for c := range conns {
		c.Conn.Close()
	}
}

type directConn struct {
	net.Conn
	s *directConns
}

func (c *directConn) Close() error {
	c.s.mu.Lock()
	delete(c.s.conns, c)
	c.s.mu.Unlock()
	return c.Conn.Close()
}
