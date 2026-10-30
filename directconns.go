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
	conns map[*directConn]struct{}
}

var errModeChanged = errors.New("the VPN setting changed while connecting")

// dial connects directly. guardLocal refuses this computer and the home
// network, checked on the address actually dialed (names that resolve
// there included).
func (s *directConns) dial(ctx context.Context, network, addr string, guardLocal bool) (net.Conn, error) {
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()
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
	if s.gen != gen {
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

// closeAll closes every direct connection, and any being made now.
func (s *directConns) closeAll() {
	s.mu.Lock()
	s.gen++
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
