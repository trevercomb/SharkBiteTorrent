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
	if _, err := s.dial(ctx, "tcp", ln.Addr().String(), true); err == nil {
		t.Error("guardLocal dialed 127.0.0.1")
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}
