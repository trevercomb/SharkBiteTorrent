//go:build darwin

package main

import (
	"strings"
	"testing"
	"time"
)

// Lines as tcpdump -i pktap,all -k NPD -nn prints them (from real captures,
// some with directions added).
const sampleCapture = `00:59:09.510576 (proc SharkBiteTorrent:16734, out) IP 192.168.4.51.59785 > 212.56.53.55.1337: UDP, length 148
00:59:09.510624 (proc SharkBiteTorrent:16734, out) IP 192.168.4.51.59785 > 212.56.53.55.1337: UDP, length 96
00:59:09.600000 (proc SharkBiteTorrent:16734, in) IP 212.56.53.55.1337 > 192.168.4.51.59785: UDP, length 1400
00:59:12.522988 (proc mDNSResponder:656, eproc dscacheutil:17455, out) IP 100.72.141.16.53153 > 100.100.100.100.53: 48106+ AAAA? leakprobe-marker.example.com. (46)
00:59:05.048311 (proc mDNSResponder:656, eproc SharkBiteTorrent:16734, out) IP 100.72.141.16.63464 > 100.100.100.100.53: 31886+ A? www.privateinternetaccess.com. (44)
00:59:05.082717 (in) IP 100.100.100.100.53 > 100.72.141.16.63464: 31886 2/0/0 A 162.159.61.4, A 172.64.41.4 (76)
00:59:05.082800 (in) IP 100.100.100.100.53 > 100.72.141.16.60000: 31886 1/0/0 A 6.6.6.6 (60)
01:47:07.656844 (out) IP 100.72.141.16.49543 > 100.100.100.100.53: 29765+ Type65? gdmf.apple.com. (32)
00:59:06.000000 (proc SharkBiteTorrent:16734, out) IP 192.168.4.51.50000 > 162.159.61.4.443: Flags [S], seq 1, win 65535, length 0
00:59:36.000000 (proc SharkBiteTorrent:16734, out) IP 192.168.4.51.50010 > 162.159.61.4.443: Flags [S], seq 1, win 65535, length 0
00:59:36.100000 (proc com.apple.WebKit.Networking:16740, out) IP6 2600:1702::69.51147 > 2a04:4e42:4c::347.443: Flags [P.], seq 1:100, ack 1, win 2048, length 99
00:59:37.000000 (proc kernel_task:0, out) IP 192.168.4.51.49152 > 192.168.4.26.445: Flags [.], ack 1, win 2048, length 0
00:59:38.000000 (proc Discord Helper:999, out) IP 192.168.4.51.50001 > 162.159.136.234.443: Flags [S], seq 1, win 65535, length 0
00:59:39.000000 (proc SharkBiteTorrent:16734, out) IP 192.168.4.51 > 8.8.8.8: ICMP echo request, id 1, seq 1, length 64
00:59:29.885458 (proc mDNSResponder:656, eproc :656, out) IP 192.168.4.1.5353 > 224.0.0.251.5353: 0 [6q] SRV (QM)? x._trel._udp.local. (60)
00:59:41.000000 (proc leakcheck:16734, in) IP6 ::1.9696 > ::1.50736: Flags [P.], seq 1:100, ack 1, win 6000, length 99
00:59:41.000001 (proc leakcheck:16734, out) IP6 ::1.50736 > ::1.9696: Flags [.], ack 100, win 6000, length 0
02:00:00.000001 (out) IP 192.168.4.51.50790 > 1.1.1.1.443: Flags [S], seq 1, win 65535, length 0
02:00:00.000002 (proc kernel_task:0, eproc leakcheck:16734, in) IP 1.1.1.1.443 > 192.168.4.51.50790: Flags [S.], seq 1, ack 2, win 65535, length 0
02:00:00.000003 (out) IP 192.168.4.51.50791 > 9.9.9.9.53: 1+ A? x. (20)
02:00:00.000004 (proc Prowlarr:700, out) IP 127.0.0.1.60000 > 127.0.0.1.49602: Flags [P.], seq 1:10, ack 1, win 6000, length 9
02:00:00.000005 (proc SharkBiteTorrent:16734, in) IP 127.0.0.1.60000 > 127.0.0.1.49602: Flags [P.], seq 1:10, ack 1, win 6000, length 9
02:00:00.000006 (out) IP 192.168.4.51.50792 > 203.0.113.9.6881: UDP, length 20
02:00:00.000007 (out) IP 192.168.4.51 > 162.159.192.8: ICMP 192.168.4.51 udp port 2408 unreachable, length 36
02:00:00.000008 (out) IP 192.168.4.51.50793 > 203.0.113.10.6881: UDP, length 20
02:00:00.000009 (proc kernel_task:0, out) IP 192.168.4.51.50794 > 203.0.113.11.80: Flags [S], seq 1, win 65535, length 0
02:00:00.000011 (out) IP 192.168.4.51.49879 > 192.168.4.26.139: Flags [S], seq 1, win 65535, length 0
02:00:00.000012 (out) IP 192.168.4.51.49880 > 203.0.113.12.445: Flags [S], seq 1, win 65535, length 0
02:00:01.000000 (proc tailscaled:300, out) IP 192.168.4.51.41641 > 198.51.100.7.41641: UDP, length 148
02:00:02.000000 (proc trustd:400, out) IP 192.168.4.51.50795 > 17.253.1.1.80: Flags [S], seq 1, win 65535, length 0
00:59:40.000000 ARP, Request who-has 192.168.4.1 tell 192.168.4.51, length 28
`

func parseSample(t *testing.T) *pktapResult {
	t.Helper()
	r := newPktapResult()
	r.day = time.Now().Add(-6 * time.Hour) // the sample's times of day fall before now
	r.day = time.Date(r.day.Year(), r.day.Month(), r.day.Day(), 0, 0, 0, 0, time.Local)
	for _, l := range strings.Split(sampleCapture, "\n") {
		r.add(l)
	}
	return r
}

func TestPktapParse(t *testing.T) {
	r := parseSample(t)
	if r.unparsed != 0 || r.withDir == 0 {
		t.Errorf("unparsed %d, with direction %d: %v", r.unparsed, r.withDir, r.unparsedSample)
	}
	k := pktapKey{proc: 16734, dir: "out", proto: "udp", src: "192.168.4.51", dst: "212.56.53.55", sport: 59785, dport: 1337}
	if s := r.counts[k]; s == nil || s.n != 2 || !s.last.After(s.first) {
		t.Errorf("tunnel packets: %+v", s)
	}
	if len(r.handshakes["212.56.53.55:1337"]) != 1 || len(r.handshakes["198.51.100.7:41641"]) != 1 {
		t.Errorf("handshakes: %v", r.handshakes)
	}
	if r.queryNames["100.100.100.100|31886|63464"] != "www.privateinternetaccess.com" || r.queryNames["100.100.100.100|29765|49543"] != "gdmf.apple.com" {
		t.Errorf("queries (Type65 included): %v", r.queryNames)
	}
	// Answers are matched by server, DNS id and port: another lookup that
	// happened to get the same id doesn't add its addresses.
	if got := r.answersByName()["www.privateinternetaccess.com"]; len(got) != 2 || got[0] != "162.159.61.4" || got[1] != "172.64.41.4" {
		t.Errorf("answers: %v", r.answersByName())
	}
	if r.procNames[16734] != "SharkBiteTorrent" || r.procNames[kernelPID] != "kernel_task" || r.procNames[999] != "Discord Helper" {
		t.Errorf("process names: %v", r.procNames)
	}
	r.add("02:00:00.000010 (proc x:1, out) IP 192.168.4.51.1 > weird")
	r.add("02:00:00.000011 IP 192.168.4.51.1 > also weird") // no metadata
	if r.unparsed != 2 {
		t.Errorf("IP lines that couldn't be read: %d counted, want 2", r.unparsed)
	}
}

func TestSplitPktapAddr(t *testing.T) {
	cases := []struct {
		in   string
		v6   bool
		addr string
		port int
		ok   bool
	}{
		{"192.168.4.51.59785", false, "192.168.4.51", 59785, true},
		{"8.8.8.8:", false, "8.8.8.8", 0, false},
		{"2600:1702::69.443", true, "2600:1702::69", 443, true},
		{"fe80::1%en0.5353", true, "fe80::1", 5353, true},
		{"ff02::fb", true, "ff02::fb", 0, false},
	}
	for _, c := range cases {
		a, p, ok := splitPktapAddr(c.in, c.v6)
		if a != c.addr || p != c.port || ok != c.ok {
			t.Errorf("%s: got %s %d %v", c.in, a, p, ok)
		}
	}
}

func TestParseMDNSLine(t *testing.T) {
	r, ok := parseMDNSLine(`2026-10-21 00:59:24.799 Df mDNSResponder[656:145d] [com.apple.mDNSResponder:Default] [R6739] DNSServiceQueryRecord START -- qname: <private> <dn:72K6SQ>, qtype: A, flags: 0x15000, interface index: 0, client pid: 16734 (SharkBiteTorren), name hash: <dn:72K6SQ>`)
	want := time.Date(2026, 10, 21, 0, 59, 24, 799e6, time.Local)
	if !ok || r.mdnsPID != 656 || r.client != 16734 || r.hash != "72K6SQ" || !r.at.Equal(want) {
		t.Errorf("parsed %+v %v", r, ok)
	}
	// Network.framework's form (what WebKit's lookups look like).
	r, ok = parseMDNSLine(`2026-10-21 00:59:24.799 Df mDNSResponder[656:145d] [com.apple.mDNSResponder:Default] [R1234] getaddrinfo start -- flags: 0xc000d000, ifindex: 0, protocols: 0, hostname: <mask.hash: 'Zm9vYmFy'>, options: 0x0, client pid: 99 (nscurl)`)
	if !ok || r.client != 99 || r.hash != "Zm9vYmFy" {
		t.Errorf("Network.framework form: %+v %v", r, ok)
	}
	// With private data shown, the name itself.
	r, ok = parseMDNSLine(`2026-10-21 00:59:24.799 Df mDNSResponder[656:145d] [com.apple.mDNSResponder:Default] [R1] DNSServiceGetAddrInfo START -- hostname: Example.COM., flags: 0x0, client pid: 5 (x)`)
	if !ok || r.hash != "example.com" || r.client != 5 {
		t.Errorf("parsed %+v %v", r, ok)
	}
	// A request whose name can't be read still counts.
	r, ok = parseMDNSLine(`2026-10-21 00:59:24.799 Df mDNSResponder[656:145d] [com.apple.mDNSResponder:Default] [R9] DNSServiceBrowse START -- regtype: <private>, domain: <private>, client pid: 7 (y)`)
	if !ok || r.client != 7 || !strings.HasPrefix(r.hash, "unreadable request: ") {
		t.Errorf("unreadable request: %+v %v", r, ok)
	}
	if _, ok := parseMDNSLine("Filtering the log data using \"process == …\""); ok {
		t.Error("parsed a non-request line")
	}
	if !logDropped.MatchString("--- 12 messages dropped ---") || logDropped.MatchString("[R1] DNSServiceQueryRecord START -- qname: <private>") {
		t.Error("the lost-messages notice isn't recognised (or a request is)")
	}
}

func TestSocketPorts(t *testing.T) {
	out := `COMMAND     PID  USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME
SharkBite 16734 shark   12u  IPv4 0x1234567890abcdef      0t0  UDP *:58017
SharkBite 16734 shark   13u  IPv6 0x1234567890abcdef      0t0  TCP [::1]:50736->[::1]:9696 (ESTABLISHED)
SharkBite 16734 shark   14u  IPv4 0x1234567890abcdef      0t0  TCP 127.0.0.1:49602 (LISTEN)
SharkBite 16735 shark   15u  IPv4 0x1234567890abcdef      0t0  TCP 192.168.4.51:50793->203.0.113.10:443 (SYN_SENT)
`
	m := socketPorts(out)
	if m["udp:58017"] != 16734 || m["tcp:50736"] != 16734 || m["tcp:49602"] != 16734 || m["tcp:50793"] != 16735 || len(m) != 4 {
		t.Errorf("ports: %v", m)
	}
}

// The app's own packets and its WebKit helper's are judged; mDNSResponder's
// (lookups are judged from its log), other programs' traffic and what
// arrives aren't. Untagged packets are claimed through their connection or
// a port a watched process had open. Documented calls only count from the
// main process while connecting. Lookups are named by the hashes learned
// from this process's own lookups, in their time windows.
func TestDarwinVerdictRules(t *testing.T) {
	r := parseSample(t)
	w := newMacWatch(16734)
	w.pids[16740] = true         // a WebKit helper launchd says the app is responsible for
	w.ports["udp:50793"] = 16734 // lsof saw the app with this port open
	local := map[string]bool{"192.168.4.51": true, "100.72.141.16": true, "2600:1702::69": true, "127.0.0.1": true, "::1": true}
	endpoints := map[string]bool{"212.56.53.55:1337": true}
	if !r.handshakeFrom("212.56.53.55:1337", w) || r.handshakeFrom("198.51.100.7:41641", w) {
		t.Error("handshakes weren't credited to the right process (Tailscale's aren't the app's)")
	}
	windows := r.connectWindows(endpoints, w)
	if len(windows) != 1 {
		t.Fatalf("connect windows: %v", windows)
	}
	flows, un := r.forWatched(w, local, map[int]bool{656: true}, 16734, windows)
	// The planted-style connection (untagged SYN, reply tagged with the
	// app) and the app's untagged UDP from a port it had open are claimed;
	// untagged or kernel-tagged traffic nobody claims is listed (the UDP to
	// .9, the kernel's SYN to .11, and SMB to an address on the internet);
	// DNS, ICMP and file sharing with the NAS on the home network (the
	// kernel's SMB, and untagged NetBIOS) are counted apart.
	listed := strings.Join(un.flows, " ")
	if un.rescued != 2 || un.dns != 2 || un.icmp != 1 || un.fileSharing != 2 || un.unclaimed != 3 || len(un.flows) != 3 ||
		!strings.Contains(listed, "203.0.113.11:80") || !strings.Contains(listed, "203.0.113.9:6881") || !strings.Contains(listed, "203.0.113.12:445") {
		t.Errorf("untagged: %+v", un)
	}

	own := 500
	t0 := time.Date(2026, 10, 21, 1, 0, 0, 0, time.Local)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	learned := []timedLookup{
		{name: "localhost", from: at(0), to: at(5)}, // answered without mDNSResponder: no request
		{name: "example.com", from: at(200), to: at(260)},
		{name: "www.privateinternetaccess.com", from: at(500), to: at(540)},
	}
	inWindow := windows[0].from.Add(time.Second)
	reqs := []mdnsRequest{
		{at: at(210), client: own, hash: "EXMPL"},
		{at: at(211), client: own, hash: "EXMPL"},
		{at: at(505), client: own, hash: "PIAHSH"},
		{at: inWindow, client: 16734, hash: "PIAHSH"},   // the app signing in to PIA: documented
		{at: inWindow, client: 16740, hash: "PIAHSH"},   // the same name from its web view: not
		{at: at(901), client: 16734, hash: "EXMPL"},     // a planted lookup
		{at: at(902), client: 16740, hash: "UNKNWN"},    // its WebKit helper, a name nobody taught us
		{at: at(903), client: 17455, hash: "OTHER1"},    // someone else
		{at: at(904), client: 16734, hash: "localhost"}, // shown in clear: answered here, not counted
	}
	lookups := watchedLookups(reqs, w, own, 16734, windows, learned, r.answersByName())
	if got := lookups["www.privateinternetaccess.com"]; len(got) != 2 {
		t.Errorf("the documented lookup wasn't named with its answers: %v", lookups)
	}
	p := policy{local: local, endpoints: endpoints, lookups: lookups,
		servers: map[string]bool{"[::1]:9696": true}} // the user's Prowlarr
	list, allowed, _, tunnel := classify(flows, p)
	if tunnel != 2 {
		t.Errorf("tunnel packets: %d (only what the app sent counts)", tunnel)
	}
	text := strings.Join(list, "\n")
	for _, want := range []string{
		"DNS lookup outside the tunnel: example.com",
		"DNS lookup outside the tunnel: a name the check doesn't know (hash UNKNWN)",
		"DNS lookup outside the tunnel: www.privateinternetaccess.com (by another process than the app's main one)",
		"TCP to 162.159.61.4:443 (where a VPN account's documented call goes, but not while it was connecting to the VPN)",
		"TCP to [2a04:4e42:4c::347]:443", "ICMP to 8.8.8.8", "TCP to 1.1.1.1:443: 1 packets", "UDP to 203.0.113.10:6881",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("not reported: %q\n%s", want, text)
		}
	}
	for _, not := range []string{"192.168.4.26", "162.159.136.234", "224.0.0.251", "100.100.100.100", "OTHER1", "localhost", "50736", "49602", "9.9.9.9", "203.0.113.11", "17.253.1.1", "6.6.6.6"} {
		if strings.Contains(text, not) {
			t.Errorf("reported %s, which should be allowed or isn't the app's:\n%s", not, text)
		}
	}
	all := strings.Join(allowed, "\n")
	if !strings.Contains(all, "lookup of www.privateinternetaccess.com (PIA sign-in)") || !strings.Contains(all, "HTTPS to 162.159.61.4:443 (PIA sign-in)") {
		t.Errorf("the documented sign-in wasn't allowed:\n%s", all)
	}
}

func TestControlsMissing(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 1, 0, 0, 0, time.Local)
	r := newPktapResult()
	r.counts[pktapKey{proc: 42, dir: "out", proto: "udp", src: "127.0.0.1", dst: "127.0.0.1", sport: 50000, dport: 9}] = &pktapStat{n: 1}
	reqs := []mdnsRequest{{at: t0.Add(10 * time.Millisecond), client: 42, hash: "CTRL1"}}
	c := &controls{
		packets: []string{"127.0.0.1:9 from 127.0.0.1:50000"},
		lookups: []timedLookup{{name: "leakcheck-a.example.com", from: t0, to: t0.Add(20 * time.Millisecond)},
			{name: "leakcheck-b.example.com", from: t0.Add(time.Minute), to: t0.Add(time.Minute + 20*time.Millisecond)}},
	}
	m := c.missing(r, reqs, 42)
	if len(m) != 1 || !strings.Contains(m[0], "leakcheck-b.example.com") {
		t.Errorf("missing: %v", m)
	}
	if m := c.missing(r, reqs, 43); len(m) != 3 {
		t.Errorf("another process's traffic counted as the controls: %v", m)
	}
}

// Another WireGuard client's handshake to the same server doesn't open a
// connecting window; the app's does.
func TestConnectWindowsOnlyTheApps(t *testing.T) {
	r := newPktapResult()
	r.day = time.Now().Add(-6 * time.Hour)
	r.day = time.Date(r.day.Year(), r.day.Month(), r.day.Day(), 0, 0, 0, 0, time.Local)
	r.add("01:00:00.000000 (proc Cloudflare WARP:900, out) IP 192.168.4.51.40000 > 162.159.192.5.2408: UDP, length 148")
	w := newMacWatch(16734)
	ep := map[string]bool{"162.159.192.5:2408": true}
	if ws := r.connectWindows(ep, w); len(ws) != 0 || r.handshakeFrom("162.159.192.5:2408", w) {
		t.Errorf("another client's handshake counted: %v", ws)
	}
	r.add("01:05:00.000000 (proc SharkBiteTorrent:16734, out) IP 192.168.4.51.40001 > 162.159.192.5.2408: UDP, length 148")
	if ws := r.connectWindows(ep, w); len(ws) != 1 {
		t.Errorf("the app's handshake didn't open a window: %v", ws)
	}
}

// A process that sent packets off this computer but was never seen running
// is the app's if it's named like a program the app starts; a WebKit
// process that exited before launchctl answered is reported if it sent any.
func TestUnseenProcesses(t *testing.T) {
	r := newPktapResult()
	r.add("01:00:00.000000 (proc open:777, out) IP 192.168.4.51.40000 > 93.184.216.34.443: Flags [S], seq 1, win 65535, length 0")
	r.add("01:00:00.000001 (proc somebody:778, out) IP 192.168.4.51.40001 > 93.184.216.34.443: Flags [S], seq 1, win 65535, length 0")
	r.add("01:00:00.000002 (proc com.apple.WebKit.Networking:779, out) IP 192.168.4.51.40002 > 93.184.216.34.443: Flags [S], seq 1, win 65535, length 0")
	w := newMacWatch(16734)
	w.seen[16734] = true
	r.claimUnseenChildren(w, map[string]bool{"192.168.4.51": true})
	if !w.has(777) || w.has(778) {
		t.Errorf("open should be watched, the other not: %v", w.pids)
	}
	w.webkitAll[779], w.gone[779] = "com.apple.WebKit.Networking", true
	w.webkitAll[780], w.gone[780] = "com.apple.WebKit.GPU", true // exited, sent nothing
	w.webkitAll[781] = "com.apple.WebKit.WebContent"             // still running, never answered for
	if got := w.unresolved(r.sentOffMachine); len(got) != 2 || !strings.Contains(strings.Join(got, " "), "779") || !strings.Contains(strings.Join(got, " "), "781") {
		t.Errorf("unresolved: %v", got)
	}
}
