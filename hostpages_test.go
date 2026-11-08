//go:build windows || (linux && cgo) || (darwin && cgo)

package main

import (
	"context"
	"net"
	"testing"
)

// The file-host window's proxy must not reach this computer or the home
// network, by address or by a name that resolves to one.
func TestHostDialRefusesLocal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	a := &App{vpn: VpnStatus{Provider: "none", State: "off"}} // direct: the dial a page could misuse
	a.direct.allow()
	for _, host := range []string{"127.0.0.1", "localhost", "::1", "192.168.1.1", "10.0.0.1", "169.254.1.1", "0.0.0.0", "localtest.me", "fe80::1%lo0"} {
		c, err := a.hostDial(context.Background(), "tcp", net.JoinHostPort(host, port))
		if err == nil {
			c.Close()
			t.Errorf("%s: connected", host)
		}
	}
}

// Through the VPN, a name is looked up first: one that points only to this
// computer or the home network is refused without trying to connect (inside
// the tunnel that would hang until it timed out), and of mixed answers only
// the public addresses are tried.
func TestDialPublic(t *testing.T) {
	answers := map[string][]string{
		"localtest.me":   {"127.0.0.1"},
		"home.example":   {"192.168.1.5", "fe80::1"},
		"mixed.example":  {"127.0.0.1", "203.0.113.7", "198.51.100.9"},
		"public.example": {"203.0.113.8"},
	}
	lookup := func(_ context.Context, host string) ([]string, error) { return answers[host], nil }
	var dialed []string
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		if addr == "203.0.113.7:443" {
			return nil, context.DeadlineExceeded // this one doesn't answer
		}
		c, _ := net.Pipe()
		return c, nil
	}
	for _, host := range []string{"localtest.me", "home.example"} {
		dialed = nil
		if c, err := dialPublic(context.Background(), lookup, dial, "tcp", host+":443"); err == nil || len(dialed) != 0 {
			if c != nil {
				c.Close()
			}
			t.Errorf("%s: err %v, dialed %q; want refused without dialing", host, err, dialed)
		}
	}
	dialed = nil
	c, err := dialPublic(context.Background(), lookup, dial, "tcp", "mixed.example:443")
	if err != nil || len(dialed) != 2 || dialed[0] != "203.0.113.7:443" || dialed[1] != "198.51.100.9:443" {
		t.Errorf("mixed: err %v, dialed %q; want the public addresses in order", err, dialed)
	}
	if c != nil {
		c.Close()
	}
}
