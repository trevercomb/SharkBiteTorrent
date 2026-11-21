package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"sharkbite/internal/tunnel"
)

// A dead tunnel (every check times out, as with PIA after its server forgot
// the key) reconnects after failedChecksBeforeReconnect failures in a row;
// a success in between starts the count again.
func TestWatchTunnelReconnectsDeadTunnel(t *testing.T) {
	results := []error{errDead, errDead, nil, errDead, errDead, errDead, nil}
	calls := 0
	check := func(context.Context) (*tunnel.Trace, error) {
		err := results[calls]
		calls++
		if err != nil {
			return nil, err
		}
		return &tunnel.Trace{IP: "203.0.113.7"}, nil
	}
	a := &App{vpn: VpnStatus{State: "connected"}}
	reconnected := make(chan struct{})
	done := make(chan struct{})
	go func() {
		a.watchTunnel(context.Background(), time.Millisecond, check, func() { close(reconnected) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchTunnel didn't return")
	}
	select {
	case <-reconnected:
	default:
		t.Fatal("no reconnect after 3 failed checks in a row")
	}
	if calls != 6 {
		t.Fatalf("reconnected after %d checks, want 6 (2 failed, 1 ok, 3 failed)", calls)
	}
	if a.vpn.Error == "" {
		t.Fatal("the failure isn't shown")
	}
}

// Occasional failures only show the warning, and a good check clears it.
func TestWatchTunnelBlipDoesntReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	check := func(context.Context) (*tunnel.Trace, error) {
		calls++
		if calls == 20 {
			cancel()
		}
		if calls%3 == 0 {
			return nil, errDead
		}
		return &tunnel.Trace{IP: "203.0.113.7"}, nil
	}
	a := &App{}
	a.watchTunnel(ctx, time.Millisecond, check, func() { t.Fatal("reconnected on isolated failures") })
	if calls < 20 {
		t.Fatalf("stopped after %d checks", calls)
	}
}

var errDead = errors.New(`Get "https://www.cloudflare.com/cdn-cgi/trace": context deadline exceeded`)
