//go:build linux

package main

import (
	"context"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testdata/nft-linux.json is real `nft -j list table` output of nftRules,
// after this traffic from the watched cgroup: 3 UDP packets to the "VPN
// endpoint" 10.9.9.2:51820, a TCP connection attempt to 10.9.9.3:443, a DNS
// query to the 127.0.0.53 stub, a UDP packet to 127.0.0.1:9 and a ping to
// 10.9.9.5.
func TestParseAndClassify(t *testing.T) {
	data, err := os.ReadFile("testdata/nft-linux.json")
	if err != nil {
		t.Fatal(err)
	}
	flows, total, err := parseNft(data)
	if err != nil {
		t.Fatal(err)
	}
	list, _, counted, tunnel := classify(flows, policy{local: map[string]bool{"10.9.9.1": true}, endpoints: map[string]bool{"10.9.9.2:51820": true}})
	if counted != total || tunnel != 3 {
		t.Errorf("counted %d of %d packets, tunnel %d", counted, total, tunnel)
	}
	want := []string{
		"DNS lookup outside the tunnel (to 127.0.0.53:53): 1 packets",
		"ICMP to 10.9.9.5: 1 packets",
		"TCP to 10.9.9.3:443: 1 packets",
		"UDP to 127.0.0.1:9 (a service on this computer other than your search server): 1 packets",
	}
	if !reflect.DeepEqual(list, want) {
		t.Errorf("leaks:\n%q\nwant\n%q", list, want)
	}
}

func TestClassifyRemote(t *testing.T) {
	flows := []flow{
		{proto: "tcp", src: "192.168.4.20", dst: "192.168.4.50", sport: 8732, dport: 50000, packets: 5}, // phone on home Wi-Fi
		{proto: "tcp", src: "100.121.116.8", dst: "100.88.1.2", sport: 8732, dport: 50001, packets: 4},  // phone over Tailscale
		{proto: "tcp", src: "192.168.4.20", dst: "8.8.8.8", sport: 8732, dport: 50002, packets: 1},      // not a phone
	}
	list, _, _, _ := classify(flows, policy{local: map[string]bool{}, endpoints: map[string]bool{"1.2.3.4:51820": true}, remotePort: "8732", remoteTailscale: true})
	want := []string{"TCP to 8.8.8.8:50002: 1 packets"}
	if !reflect.DeepEqual(list, want) {
		t.Errorf("leaks %q, want %q", list, want)
	}
}

// Direct traffic is allowed to the user's search server (local or on the
// home network) and from the app's search proxy, nothing else local.
func TestClassifyLocalServices(t *testing.T) {
	p := policy{
		local: map[string]bool{"192.168.4.20": true}, endpoints: map[string]bool{"1.2.3.4:51820": true},
		servers: map[string]bool{"127.0.0.1:9696": true, "192.168.4.26:9117": true},
		listen:  map[int]bool{40123: true},
	}
	flows := []flow{
		{proto: "tcp", src: "127.0.0.1", dst: "127.0.0.1", sport: 50000, dport: 9696, packets: 3},       // Prowlarr here
		{proto: "tcp", src: "192.168.4.20", dst: "192.168.4.26", sport: 50001, dport: 9117, packets: 2}, // Jackett on the NAS
		{proto: "tcp", src: "127.0.0.1", dst: "127.0.0.1", sport: 40123, dport: 50002, packets: 4},      // proxy replying
		{proto: "tcp", src: "127.0.0.1", dst: "127.0.0.1", sport: 50003, dport: 8118, packets: 1},       // a local HTTP proxy
	}
	list, _, _, _ := classify(flows, p)
	want := []string{"TCP to 127.0.0.1:8118 (a service on this computer other than your search server): 1 packets"}
	if !reflect.DeepEqual(list, want) {
		t.Errorf("leaks %q, want %q", list, want)
	}
}

// A VPN account's documented direct calls while connecting pass: lookups of
// the documented hosts, HTTPS to the addresses they returned, and PIA's key
// registration with the VPN server. Other names, HTTPS to other addresses
// (even on a documented port), and port 1337 elsewhere don't.
func TestClassifyDocumentedDirect(t *testing.T) {
	p := policy{
		local: map[string]bool{}, endpoints: map[string]bool{"151.241.119.210:1337": true},
		nameservers: map[string]bool{"127.0.0.53": true},
		lookups: map[string][]string{
			"www.privateinternetaccess.com": {"172.64.147.163", "2606:4700::6810:7b60"},
			"serverlist.piaservers.net":     {"104.19.240.167"},
			"tracker.example.org":           {"203.0.113.9"},
		},
	}
	flows := []flow{
		{proto: "udp", src: "127.0.0.1", dst: "127.0.0.53", sport: 40000, dport: 53, packets: 6},
		{proto: "udp", src: "192.168.4.20", dst: "151.241.119.210", sport: 40001, dport: 1337, packets: 50}, // the tunnel
		{proto: "tcp", src: "192.168.4.20", dst: "151.241.119.210", sport: 40002, dport: 1337, packets: 11},
		{proto: "tcp", src: "192.168.4.20", dst: "172.64.147.163", sport: 40003, dport: 443, packets: 10},
		{proto: "tcp", src: "2001:db8::2", dst: "2606:4700::6810:7b60", sport: 40004, dport: 443, packets: 10},
		{proto: "tcp", src: "192.168.4.20", dst: "203.0.113.9", sport: 40005, dport: 443, packets: 3},   // resolved, not documented
		{proto: "tcp", src: "192.168.4.20", dst: "172.64.147.163", sport: 40006, dport: 80, packets: 2}, // documented address, other port
		{proto: "tcp", src: "192.168.4.20", dst: "198.51.100.7", sport: 40007, dport: 1337, packets: 2}, // not the VPN server
		{proto: "udp", src: "192.168.4.20", dst: "8.8.8.8", sport: 40008, dport: 53, packets: 1},        // DNS around the system resolver
	}
	list, allowed, counted, tunnel := classify(flows, p)
	want := []string{
		"DNS lookup outside the tunnel (to 8.8.8.8:53): 1 packets",
		"DNS lookup outside the tunnel: tracker.example.org",
		"TCP to 172.64.147.163:80: 2 packets",
		"TCP to 198.51.100.7:1337: 2 packets",
		"TCP to 203.0.113.9:443: 3 packets",
	}
	if !reflect.DeepEqual(list, want) {
		t.Errorf("leaks:\n%q\nwant\n%q", list, want)
	}
	if counted != 95 || tunnel != 50 || len(allowed) != 5 {
		t.Errorf("counted %d, tunnel %d, allowed %q", counted, tunnel, allowed)
	}
}

// DNS to the system resolver that the check's recorder never saw (the
// redirect didn't work) can't be judged by name, so it's reported.
func TestClassifyUnseenDNS(t *testing.T) {
	p := policy{local: map[string]bool{}, endpoints: map[string]bool{"1.2.3.4:51820": true}, nameservers: map[string]bool{"127.0.0.53": true}}
	list, _, _, _ := classify([]flow{{proto: "udp", src: "127.0.0.1", dst: "127.0.0.53", sport: 40000, dport: 53, packets: 2}}, p)
	if len(list) != 1 || !strings.Contains(list[0], "not seen") {
		t.Errorf("leaks %q", list)
	}
}

// The recorder passes queries on and records names and answers, over UDP
// and TCP.
func TestDNSLogger(t *testing.T) {
	ns := systemNameservers()
	if len(ns) == 0 {
		t.Skip("no IPv4 nameserver in /etc/resolv.conf")
	}
	l, err := startDNSLogger(ns[0])
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	u, tc := l.ports()
	lookup := func(network string, port int, name string) error {
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		}}
		_, err := r.LookupIPAddr(t.Context(), name+".")
		return err
	}
	lookup("udp", u, "localhost") // answered or not, the name is recorded
	if _, ok := l.lookups()["localhost"]; !ok {
		t.Errorf("UDP lookup not recorded: %v", l.lookups())
	}

	// The TCP part needs the real nameserver to answer over TCP, which
	// GitHub's runners stopped doing (Azure's 168.63.129.16: reset or EOF,
	// 2026-10-25); only the recorder is under test, so check that first.
	if !answersTCP(t, ns[0]) {
		t.Logf("skipping the TCP part: %s doesn't answer DNS over TCP here", ns[0])
		return
	}
	if err := lookup("tcp", tc, "one.one.one.one"); err != nil {
		t.Errorf("tcp: %v", err)
	}
	if len(l.lookups()["one.one.one.one"]) == 0 {
		t.Errorf("TCP lookup's answers not recorded: %v", l.lookups())
	}
}

// answersTCP reports whether the nameserver answers a lookup over TCP.
func answersTCP(t *testing.T, server string) bool {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(server, "53"))
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := r.LookupIPAddr(ctx, "one.one.one.one.")
	return err == nil
}

// Windows' DNS Client event 3008 lists answers like this.
func TestParseQueryResults(t *testing.T) {
	got := parseQueryResults("type:  5 www.cloudflare.com.cdn.cloudflare.net;::ffff:104.16.123.96;::ffff:104.16.124.96;2606:4700::6810:7b60;")
	want := []string{"104.16.123.96", "104.16.124.96", "2606:4700::6810:7b60"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%q, want %q", got, want)
	}
}
