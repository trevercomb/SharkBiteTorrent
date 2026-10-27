//go:build linux || darwin

package main

// What Linux and macOS share: how counted traffic is judged (the rules are
// the same; only how each OS attributes packets to the app differs, see
// linux.go and darwin.go), the engine run as a child process, and sudo.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"sharkbite/internal/secret"
	"sharkbite/internal/systool"
	"sharkbite/internal/warp"
)

// flow is one counted set element: packets the watched cgroup sent.
type flow struct {
	proto              string // tcp, udp or an IP protocol name
	src, dst           string // IP addresses
	sport, dport       int
	packets, bytesSent int64
	// notDocumented, when set, says why this flow can't be one of a VPN
	// account's documented direct calls even if it goes where they do (macOS
	// knows the process and the time: "not from the app's main process",
	// "not while connecting"). Linux can't tell, and leaves it empty.
	notDocumented string
}

func (f flow) to() string { return net.JoinHostPort(f.dst, strconv.Itoa(f.dport)) }

// policy is what the watched processes may contact outside the tunnel.
type policy struct {
	local           map[string]bool // this computer's addresses
	endpoints       map[string]bool // the VPN servers (UDP)
	remotePort      string          // the remote control, if on
	remoteTailscale bool
	servers         map[string]bool // the user's Prowlarr/Jackett, ip:port
	listen          map[int]bool    // the app's own local listeners (search proxy)
	nameservers     map[string]bool // the system's, whose queries the check recorded
	lookups         map[string][]string
	// recordedUDP is how many UDP queries the check's DNS recorder got
	// (Linux; nil elsewhere). Every UDP packet to the system's nameservers
	// should have reached it; more packets than that went around it.
	recordedUDP *int64
	engineRun   bool // the engine test (not -app): it downloads a torrent the whole time
}

// minTunnelPackets is about 5 MB through the tunnel. Less, and the run
// barely downloaded anything, so a PASS wouldn't show much: on 2026-10-21
// the engine test passed with 354 packets while its trackerless torrent
// found no peers.
const minTunnelPackets = 5000

// documented returns the addresses the documented direct calls' lookups
// returned (HTTPS to them is allowed), and the VPN servers' IPs (PIA's key
// registration goes there).
func (p policy) documented() (https map[string]string, servers map[string]bool) {
	https, servers = map[string]string{}, map[string]bool{}
	for name, addrs := range p.lookups {
		if why, ok := documentedDirect[name]; ok {
			for _, a := range addrs {
				https[a] = why
			}
		}
	}
	for ep := range p.endpoints {
		if h, _, err := net.SplitHostPort(ep); err == nil {
			servers[h] = true
		}
	}
	return https, servers
}

// appLocalServices reads which local services the app uses by design: its
// search server (contacted directly when it's local) and its search proxy's
// port (Prowlarr connects to it, so the app answers from it).
func appLocalServices() (servers map[string]bool, listen map[int]bool) {
	servers, listen = map[string]bool{}, map[int]bool{}
	cfgDir, _ := os.UserConfigDir()
	dir := filepath.Join(cfgDir, "SharkBiteTorrent")
	var sc struct {
		URL string `json:"url"`
	}
	if data, err := secret.ReadFile(filepath.Join(dir, "search.json")); err == nil && json.Unmarshal(data, &sc) == nil {
		if u, err := url.Parse(sc.URL); err == nil && u.Hostname() != "" {
			port := u.Port()
			if port == "" {
				port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
			}
			ips, _ := (&net.Resolver{}).LookupIPAddr(context.Background(), u.Hostname())
			for _, ip := range ips {
				// Only a local server may be contacted directly (a remote one,
				// such as a seedbox, must go through the VPN; see search.serverDial).
				if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || tailscaleAddr(net.JoinHostPort(ip.IP.String(), "0")) {
					servers[net.JoinHostPort(ip.IP.String(), port)] = true
				}
			}
		}
	}
	var pc struct {
		Enabled bool `json:"enabled"`
		Port    int  `json:"port"`
	}
	if data, err := secret.ReadFile(filepath.Join(dir, "proxy.json")); err == nil && json.Unmarshal(data, &pc) == nil && pc.Enabled {
		listen[pc.Port] = true
	}
	return servers, listen
}

// runChild is the engine run inside the watched cgroup.
func runChild(warpFile string, secs int, selftest bool) int {
	data, err := os.ReadFile(warpFile)
	if err != nil {
		return fatalCode(err.Error())
	}
	var acc *warp.Account
	if err := json.Unmarshal(data, &acc); err != nil || acc == nil {
		return fatalCode("reading the test WARP account: " + errText(err))
	}
	if err := runEngine(acc, secs, selftest, flag.Args()[0]); err != nil {
		return fatalCode(err.Error())
	}
	return 0
}

func verdict(flows []flow, total int64, pol policy, selftest bool) int {
	list, allowed, counted, tunnel := classify(flows, pol)
	fmt.Printf("\npackets from the watched processes: %d (tunnel %d)\n", total, tunnel)
	if counted != total {
		// The per-address sets missed packets (full?), so the details are
		// incomplete; the total counter can't miss.
		fmt.Printf("FAIL: %d packets weren't attributed to an address, so they couldn't be checked\n", total-counted)
		return 1
	}
	if tunnel == 0 {
		fmt.Println("FAIL: the VPN tunnel's own traffic (UDP to " + setString(pol.endpoints) + ") wasn't counted, so nothing was verified")
		return 1
	}
	if selftest {
		missed := []string{}
		if !containsAny(list, plantedConn) {
			missed = append(missed, "the direct connection to "+plantedConn)
		}
		if !containsAny(list, "DNS lookup outside the tunnel: "+plantedLookup) {
			missed = append(missed, "the system DNS lookup of "+plantedLookup)
		}
		if len(missed) > 0 {
			fmt.Println("selftest FAILED: the check missed", strings.Join(missed, " and "))
			return 1
		}
		fmt.Println("selftest OK: caught the planted leaks:")
		for _, l := range list {
			fmt.Println("  ", l)
		}
		return 0
	}
	if len(list) > 0 {
		fmt.Println("FAIL: traffic outside the tunnel:")
		for _, l := range list {
			fmt.Println("  ", l)
		}
		return 1
	}
	if tunnel < minTunnelPackets {
		if pol.engineRun {
			fmt.Printf("INCONCLUSIVE: nothing leaked, but only %d packets went through the tunnel: the torrent barely downloaded (no peers found?), so its traffic wasn't really tested. Run it again, or with another torrent.\n", tunnel)
			return 1
		}
		fmt.Printf("note: only %d packets went through the tunnel. If you added a download, it didn't get going, so torrent traffic wasn't really tested; run it again with one downloading.\n", tunnel)
	}
	if len(allowed) > 0 {
		fmt.Println("PASS: only the VPN tunnel, this computer, and the VPN account's documented calls while connecting:")
		for _, l := range allowed {
			fmt.Println("  ", l)
		}
		return 0
	}
	fmt.Println("PASS: only the VPN tunnel and this computer were contacted; no system DNS lookups")
	return 0
}

// classify sorts the counted flows into the tunnel, allowed traffic and
// leaks (returned as text with packet counts).
func classify(flows []flow, p policy) (list, allowed []string, counted, tunnel int64) {
	type leak struct {
		text    string
		packets int64
	}
	leaks := map[string]*leak{}
	add := func(text string, n int64) {
		if leaks[text] == nil {
			leaks[text] = &leak{text: text}
		}
		leaks[text].packets += n
	}
	https, vpnServers := p.documented()
	// TCP conversations on this computer whose both ends sent counted
	// packets, i.e. both ends are watched processes: the app's file-host
	// window talking to the app's own proxy, say. A local service outside
	// the app (a system proxy) sends nothing that's counted, so traffic to
	// it still shows up below.
	sent := map[string]bool{}
	for _, f := range flows {
		if f.proto == "tcp" {
			sent[net.JoinHostPort(f.src, strconv.Itoa(f.sport))+">"+f.to()] = true
		}
	}
	internal := func(f flow) bool {
		return f.proto == "tcp" && (p.local[f.dst] || net.ParseIP(f.dst).IsLoopback()) &&
			sent[f.to()+">"+net.JoinHostPort(f.src, strconv.Itoa(f.sport))]
	}
	var systemDNS, systemDNSUDP int64
	for _, f := range flows {
		counted += f.packets
		switch {
		case f.dport == 53 && (f.proto == "udp" || f.proto == "tcp") && p.nameservers[f.dst]:
			systemDNS += f.packets // judged by name, below
			if f.proto == "udp" {
				systemDNSUDP += f.packets
			}
		case f.dport == 53 && (f.proto == "udp" || f.proto == "tcp"):
			add("DNS lookup outside the tunnel (to "+f.to()+")", f.packets)
		case f.proto == "tcp" && f.dport == 443 && https[f.dst] != "" && f.notDocumented == "":
			// A VPN account's documented direct call (connecting).
			allowed = append(allowed, "HTTPS to "+f.to()+" ("+https[f.dst]+")")
		case f.proto == "tcp" && f.dport == piaKeyPort && vpnServers[f.dst] && f.notDocumented == "":
			allowed = append(allowed, "TCP to "+f.to()+" (PIA key registration with the VPN server)")
		case f.proto == "tcp" && p.servers[normalizeHostPort(f.to())]:
			// The user's own Prowlarr/Jackett, on this computer or the home
			// network (contacted directly by design).
		case (p.local[f.dst] || net.ParseIP(f.dst).IsLoopback()) && p.listen[f.sport]:
			// The app's search proxy answering Prowlarr.
		case f.proto == "udp" && p.endpoints[normalizeHostPort(f.to())]:
			tunnel += f.packets
		case p.remotePort != "" && f.proto == "tcp" && strconv.Itoa(f.sport) == p.remotePort &&
			(privateAddr(f.to()) && !tailscaleAddr(net.JoinHostPort(f.src, "0")) ||
				p.remoteTailscale && tailscaleAddr(f.to()) && tailscaleAddr(net.JoinHostPort(f.src, "0"))):
			// The remote control (on in Settings) answering a phone.
		case internal(f):
			// Between the app's own processes.
		case p.local[f.dst] || net.ParseIP(f.dst).IsLoopback():
			// Anything else on this computer: a system proxy or DNS forwarder
			// listening there would pass traffic on outside the tunnel.
			add(strings.ToUpper(f.proto)+" to "+f.to()+" (a service on this computer other than your search server)", f.packets)
		default:
			text := strings.ToUpper(f.proto) + " to " + f.dst
			if f.proto == "tcp" || f.proto == "udp" {
				text = strings.ToUpper(f.proto) + " to " + f.to()
			}
			if f.notDocumented != "" && (https[f.dst] != "" || vpnServers[f.dst]) {
				text += " (where a VPN account's documented call goes, but " + f.notDocumented + ")"
			}
			add(text, f.packets)
		}
	}
	for _, l := range leaks {
		list = append(list, fmt.Sprintf("%s: %d packets", l.text, l.packets))
	}
	names := 0
	for name := range p.lookups {
		if why, ok := documentedDirect[name]; ok {
			allowed = append(allowed, "lookup of "+name+" ("+why+")")
		} else {
			list = append(list, "DNS lookup outside the tunnel: "+name)
		}
		names++
	}
	if p.recordedUDP != nil && systemDNSUDP > *p.recordedUDP {
		// Redirected queries all reach the recorder; these went around it.
		list = append(list, fmt.Sprintf("DNS to the system's nameservers that didn't reach the check's recorder: %d of %d UDP packets", systemDNSUDP-*p.recordedUDP, systemDNSUDP))
	}
	if systemDNS > 0 && names == 0 {
		// Sent to the system's resolver but never reached the check's: the
		// names weren't seen, so they couldn't be judged.
		list = append(list, fmt.Sprintf("DNS lookup outside the tunnel (not seen by the check's recorder): %d packets", systemDNS))
	}
	sort.Strings(list)
	sort.Strings(allowed)
	return list, dedupe(allowed), counted, tunnel
}

// localAddrs are this computer's own addresses (loopback included).
func localAddrs() map[string]bool {
	m := map[string]bool{}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			m[ipn.IP.String()] = true
		}
	}
	for _, s := range []string{"127.0.0.1", "::1"} {
		m[s] = true
	}
	return m
}

func mustFind(name string) string {
	if p, ok := systool.Find(name); ok {
		return p
	}
	return name
}

func sudo(args ...string) ([]byte, error) {
	return systool.Command("sudo", append([]string{"-n"}, args...)...).CombinedOutput()
}

func sudoInput(input string, args ...string) ([]byte, error) {
	cmd := systool.Command("sudo", append([]string{"-n"}, args...)...)
	cmd.Stdin = strings.NewReader(input)
	return cmd.CombinedOutput()
}

func sudoInteractive(args ...string) error {
	cmd := systool.Command("sudo", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
