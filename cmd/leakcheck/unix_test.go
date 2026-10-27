//go:build linux || darwin

package main

import (
	"strings"
	"testing"
)

// UDP queries to the system's nameservers that outnumber what the check's
// DNS recorder received went around it, and are reported.
func TestRecordedUDPShortfall(t *testing.T) {
	recorded := int64(2)
	p := policy{local: map[string]bool{}, endpoints: map[string]bool{"1.2.3.4:51820": true},
		nameservers: map[string]bool{"127.0.0.53": true},
		lookups:     map[string][]string{"example.org": nil}, recordedUDP: &recorded}
	flows := []flow{
		{proto: "udp", src: "10.0.0.2", dst: "127.0.0.53", sport: 40000, dport: 53, packets: 5},
		{proto: "tcp", src: "10.0.0.2", dst: "127.0.0.53", sport: 40001, dport: 53, packets: 9}, // TCP DNS isn't compared
	}
	list, _, _, _ := classify(flows, p)
	if !strings.Contains(strings.Join(list, "\n"), "didn't reach the check's recorder: 3 of 5 UDP packets") {
		t.Errorf("shortfall not reported:\n%s", strings.Join(list, "\n"))
	}
	recorded = 5
	list, _, _, _ = classify(flows, p)
	if strings.Contains(strings.Join(list, "\n"), "didn't reach") {
		t.Errorf("reported a shortfall with every packet recorded:\n%s", strings.Join(list, "\n"))
	}
}

// A flow to where a documented direct call goes isn't allowed when macOS
// says it can't be one (wrong process, or not while connecting).
func TestNotDocumented(t *testing.T) {
	p := policy{local: map[string]bool{}, endpoints: map[string]bool{"5.6.7.8:1337": true},
		lookups: map[string][]string{"www.cloudflare.com": {"104.16.1.1"}}}
	flows := []flow{
		{proto: "tcp", src: "10.0.0.2", dst: "104.16.1.1", sport: 50000, dport: 443, packets: 10},
		{proto: "tcp", src: "10.0.0.2", dst: "104.16.1.1", sport: 50001, dport: 443, packets: 7, notDocumented: "not while connecting"},
		{proto: "tcp", src: "10.0.0.2", dst: "5.6.7.8", sport: 50002, dport: 1337, packets: 3, notDocumented: "not from the app's main process"},
	}
	list, allowed, _, _ := classify(flows, p)
	text := strings.Join(list, "\n")
	for _, want := range []string{"TCP to 104.16.1.1:443 (where a VPN account's documented call goes, but not while connecting)",
		"TCP to 5.6.7.8:1337 (where a VPN account's documented call goes, but not from the app's main process)"} {
		if !strings.Contains(text, want) {
			t.Errorf("not reported: %q\n%s", want, text)
		}
	}
	if a := strings.Join(allowed, "\n"); !strings.Contains(a, "HTTPS to 104.16.1.1:443") {
		t.Errorf("the documented call itself wasn't allowed:\n%s", a)
	}
}

// A clean run that barely used the tunnel is inconclusive for the engine
// test (its torrent didn't download), and only a note for -app.
func TestVerdictLittleTraffic(t *testing.T) {
	flows := []flow{{proto: "udp", src: "192.168.4.20", dst: "1.2.3.4", sport: 40000, dport: 51820, packets: 300}}
	pol := policy{local: map[string]bool{}, endpoints: map[string]bool{"1.2.3.4:51820": true}}
	if verdict(flows, 300, pol, false) != 0 {
		t.Error("-app run with little traffic failed")
	}
	pol.engineRun = true
	if verdict(flows, 300, pol, false) == 0 {
		t.Error("engine run that barely downloaded passed")
	}
	flows[0].packets = minTunnelPackets
	if verdict(flows, minTunnelPackets, pol, false) != 0 {
		t.Error("engine run with enough traffic didn't pass")
	}
}

// Local TCP between two watched processes (the file-host window and the
// app's proxy) is allowed: both directions are counted. To a local service
// outside the app (only one direction counted) it's still reported.
func TestClassifyBetweenOwnProcesses(t *testing.T) {
	pol := policy{local: map[string]bool{}, endpoints: map[string]bool{"1.2.3.4:51820": true}}
	flows := []flow{
		{proto: "tcp", src: "127.0.0.1", dst: "127.0.0.1", sport: 50000, dport: 41000, packets: 30}, // window -> app's proxy
		{proto: "tcp", src: "127.0.0.1", dst: "127.0.0.1", sport: 41000, dport: 50000, packets: 40}, // proxy -> window
		{proto: "tcp", src: "127.0.0.1", dst: "127.0.0.1", sport: 50001, dport: 8118, packets: 5},   // to a system proxy
	}
	list, _, _, _ := classify(flows, pol)
	if len(list) != 1 || !strings.Contains(list[0], "127.0.0.1:8118") {
		t.Errorf("leaks %q, want only the system proxy", list)
	}
}
