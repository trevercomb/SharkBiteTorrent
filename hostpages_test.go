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
