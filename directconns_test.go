package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// Switching the VPN mode closes connections made without it.
func TestDirectConnsCloseAll(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, c) // holds it open until the client closes
		}
	}()

	var s directConns
	ctx := context.Background()
	if _, err := s.dial(ctx, "tcp", ln.Addr().String(), false); err != errModeChanged {
		t.Fatalf("before No VPN is up: %v, want refused", err)
	}
	s.allow()
	a, err := s.dial(ctx, "tcp", ln.Addr().String(), false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.dial(ctx, "tcp", ln.Addr().String(), false)
	if err != nil {
		t.Fatal(err)
	}
	b.Close() // closed by its user: no longer tracked
	if len(s.conns) != 1 {
		t.Fatalf("%d tracked, want 1", len(s.conns))
	}

	s.closeAll()
	a.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := a.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Errorf("read after closeAll: %v, want closed", err)
	}
	// While the mode changes (the engine shutting down), nothing new: a
	// caller that had just decided on a direct connection gets refused.
	if c, err := s.dial(ctx, "tcp", ln.Addr().String(), false); err != errModeChanged {
		if c != nil {
			c.Close()
		}
		t.Errorf("after closeAll: %v, want refused", err)
	}
	s.allow()
	if _, err := s.dial(ctx, "tcp", ln.Addr().String(), true); err == nil {
		t.Error("guardLocal dialed 127.0.0.1")
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}
