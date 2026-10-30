//go:build linux

package main

// Linux: the watched processes run in their own cgroup (a systemd scope),
// and an nftables table counts every packet any process in that cgroup
// sends, keyed by addresses, protocol and ports. The kernel matches packets
// to the cgroup through their socket, so every TCP and UDP packet counts,
// however short-lived the connection, including UDP sent without connect()
// and traffic from every process the app starts (WebKit's helpers).
//
// System DNS: glibc normally asks systemd-resolved over a Unix socket
// (nss-resolve), which sends no packet from the app. The watched processes
// therefore get a private /etc/nsswitch.conf ("files myhostname dns"), so
// any system lookup becomes a DNS packet, to the 127.0.0.53 stub or a DNS
// server, and is counted like everything else. The answers are the same.
// /run/nscd is hidden from them too, so glibc can't hand lookups to nscd.
// Their packets to the system's nameservers are redirected to a resolver in
// this check (dnslog.go), which records each name and passes the query on,
// so each lookup is judged by name: only the documented direct calls of a
// VPN account (documentedDirect) are allowed, and HTTPS only to the
// addresses those lookups returned.
//
// Not seen, by design: work other processes do on the app's behalf, outside
// its cgroup (D-Bus calls to systemd-resolved, desktop portals, avahi). And
// UDP to the WireGuard endpoint is allowed from any watched process, not
// only the main one as on Windows.
//
// It runs as the logged-in user and uses sudo (asked once) for nft and for
// the private mount; the watched processes run as the user, with the user's
// environment, so the app finds its keyring and the desktop as usual.
//
// Allowed: traffic to this computer (except DNS port 53), UDP to the VPN
// server's WireGuard endpoint, the remote control's replies to the home
// network or Tailscale, and a VPN account's documented direct calls while
// connecting. Everything else is reported.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"sharkbite/internal/systool"
	"sharkbite/internal/warp"
)

const (
	nftTable     = "sbt_leakcheck"
	linuxAppComm = "SharkBiteTorren" // the kernel's 15-character process name
	linuxExe     = "/usr/bin/sharkbitetorrent"
)

func runLinux(secs int, selftest, app bool, exe, endpointFlag, childWarp string) int {
	if childWarp != "" {
		return runChild(childWarp, secs, selftest)
	}
	if exe == defaultExe() {
		exe = linuxExe
	}
	if secs == 0 {
		secs = 60
		if app {
			secs = 150
		} else if selftest {
			secs = 10
		}
	}

	var endpoint, remotePort string
	var remoteTailscale bool
	var childArgs []string
	if app {
		if appRunning() {
			return fatalCode("quit Shark BiteTorrent first (tray icon → Quit): this check launches it itself, to cover its startup")
		}
		if _, err := os.Stat(exe); err != nil {
			return fatalCode("app not found at " + exe + " (use -exe)")
		}
		endpoint = endpointFlag
		remotePort, remoteTailscale = appRemotePort()
		if endpoint == "" {
			var err error
			if endpoint, err = appEndpoint(); errors.Is(err, errAccountEndpoint) {
				fmt.Println("The app uses a VPN account; the server it connects to is read from the app afterwards.")
			} else if err != nil {
				return fatalCode(err.Error())
			}
		}
		childArgs = []string{exe}
	} else {
		if len(flag.Args()) != 1 {
			return fatalCode("usage: leakcheck [-selftest] [-seconds N] <magnet|file.torrent>   or   leakcheck -app")
		}
		// A throwaway WARP account, registered here, outside the watched
		// cgroup: registering is a direct API call by design.
		tmp, err := os.MkdirTemp("", "leakcheck")
		if err != nil {
			return fatalCode(err.Error())
		}
		defer os.RemoveAll(tmp)
		// Kept as plain JSON in the private temp folder, not through the
		// app's secret storage, which could create its key file.
		acc, err := warp.Register()
		if err != nil {
			return fatalCode("registering a test WARP account: " + err.Error())
		}
		warpFile := filepath.Join(tmp, "warp.json")
		data, _ := json.Marshal(acc)
		if err := os.WriteFile(warpFile, data, 0o600); err != nil {
			return fatalCode(err.Error())
		}
		endpoint = acc.Endpoint
		self, err := os.Executable()
		if err != nil {
			return fatalCode(err.Error())
		}
		childArgs = []string{self, "-child", warpFile, "-seconds", strconv.Itoa(secs)}
		if selftest {
			childArgs = append(childArgs, "-selftest")
		}
		childArgs = append(childArgs, flag.Args()[0])
	}
	if endpoint != "" {
		endpoint = normalizeHostPort(endpoint)
	}

	readEndpoints := func() []string { return nil }
	if app {
		readEndpoints = startEndpointRecording()
	}
	defer func() { stopEndpointRecording() }()

	fmt.Println("YOU: if sudo asks for your password, type it (it's needed for the firewall counters and the private DNS setup).")
	if err := sudoInteractive("-v"); err != nil {
		return fatalCode("sudo: " + err.Error())
	}
	sudo("nft", "delete", "table", "inet", nftTable) // left over from an interrupted run
	// Keep sudo's timestamp fresh, so reading the counters and removing the
	// table at the end still work after a long run.
	keepAlive := time.NewTicker(60 * time.Second)
	defer keepAlive.Stop()
	go func() {
		for range keepAlive.C {
			sudo("-v")
		}
	}()

	nss, err := os.CreateTemp("", "leakcheck-nsswitch")
	if err != nil {
		return fatalCode(err.Error())
	}
	defer os.Remove(nss.Name())
	nss.WriteString("hosts: files myhostname dns\n")
	nss.Close()
	os.Chmod(nss.Name(), 0o644)

	// System lookups by name (see the top of this file).
	nameservers := systemNameservers()
	var dns *dnsLogger
	if len(nameservers) > 0 {
		if dns, err = startDNSLogger(nameservers[0]); err != nil {
			return fatalCode("starting the DNS recorder: " + err.Error())
		}
		defer dns.close()
	}

	// The watched command: in a new systemd scope (its own cgroup), as root
	// only long enough to bind-mount the private nsswitch.conf in a private
	// mount namespace, then waiting for "go" (sent once the counters are in
	// place) and dropping back to this user.
	unit := fmt.Sprintf("sbt-leakcheck-%d", os.Getpid())
	home, _ := os.UserHomeDir()
	// $1 nsswitch file, $2 uid, $3 gid, $4 home, $5 USER=…, then the command;
	// env puts HOME and USER back after sudo. The mount namespace is a slave:
	// its own mounts (the nsswitch.conf bind, nscd hidden) stay inside, while
	// mounts made outside reach it, such as a network drive's automount
	// (x-systemd.automount) mounting when the app first uses it. With a
	// private namespace that mount never showed up inside, so the app
	// couldn't use its download folder there.
	script := `mount --bind "$1" /etc/nsswitch.conf || exit 1
for d in /run/nscd /var/run/nscd; do if [ -d "$d" ] && ! mountpoint -q "$d"; then mount -t tmpfs tmpfs "$d" || exit 1; fi; done
read go || exit 1; exec setpriv --reuid="$2" --regid="$3" --init-groups --inh-caps=-all -- env HOME="$4" "$5" "${@:6}"`
	args := []string{"--user", "--scope", "--quiet", "--unit=" + unit, "--",
		mustFind("sudo"), "-n", "-E", "--", mustFind("unshare"), "--mount", "--propagation", "slave", "--",
		mustFind("bash"), "-c", script, "leakcheck", nss.Name(),
		strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid()), home, "USER=" + os.Getenv("USER")}
	args = append(args, childArgs...)
	cmd := systool.Command("systemd-run", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fatalCode(err.Error())
	}
	var appLog bytes.Buffer
	if app {
		cmd.Stdout, cmd.Stderr = &appLog, &appLog
	} else {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // Ctrl+C reaches only us; we stop the scope
	if err := cmd.Start(); err != nil {
		return fatalCode("starting the watched process: " + err.Error())
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	cg, err := scopeCgroup(cmd.Process.Pid, unit)
	if err != nil {
		sudo("kill", "-KILL", strconv.Itoa(cmd.Process.Pid))
		return fatalCode(err.Error())
	}
	// Stops everything in the scope, root-owned parts included (sudo may put
	// the command in a session of its own, so a process-group kill can miss).
	stop := func() {
		sudo("sh", "-c", "echo 1 > /sys/fs/cgroup/"+cg+"/cgroup.kill")
	}
	if out, err := sudoInput(nftRules(cg, nameservers, dns), "nft", "-f", "-"); err != nil {
		stop()
		if msg := staleKernel(); msg != "" {
			return fatalCode(msg)
		}
		return fatalCode("installing the nftables counters: " + err.Error() + ": " + strings.TrimSpace(string(out)))
	}
	cleanup := func() {
		if out, err := sudo("nft", "delete", "table", "inet", nftTable); err != nil && !bytes.Contains(out, []byte("No such file")) {
			fmt.Fprintf(os.Stderr, "leakcheck: couldn't remove the counters (%v); remove them with: sudo nft delete table inet %s\n", err, nftTable)
		}
	}
	defer cleanup()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		fmt.Println("\ninterrupted")
		stop()
		cleanup()
		stopEndpointRecording()
		os.Exit(130)
	}()

	fmt.Fprintln(stdin, "go")
	stdin.Close()
	start := time.Now()

	if app {
		fmt.Printf("Started Shark BiteTorrent; watching it for %d seconds.\nYOU: leave the window alone for the first minute (that covers startup). Then: search and add a result; type or paste in the magnet box; open every Settings section (type in a text field, click through VPN providers without saving); turn the search proxy on and click Check it's working (then off again if you don't use it); switch to another VPN provider and save, then back (that covers reconnecting); click Check for updates; and paste https://trevercomb.github.io/SharkBiteTorrent/leaktest/ in Paste links, tick the link it finds (Leak test page) and add it: its browser window opens the page by itself, and after about 10 seconds the small file sbt-leaktest.bin appears in your downloads (that shows the page ran; remove it afterwards).\n", secs)
		select {
		case err := <-exited:
			return fatalCode(fmt.Sprintf("the app exited after %ds (%v): %s", int(time.Since(start).Seconds()), err, lastLines(appLog.String(), 10)))
		case <-time.After(time.Duration(secs) * time.Second):
		}
		// Quitting is covered too: the person running the check quits it, the
		// way users do.
		fmt.Println("\nYOU: now quit Shark BiteTorrent from its tray icon (right-click → Quit). The copy this check started used a private DNS setup; start it again afterwards (the usual way).")
		select {
		case <-exited:
		case <-time.After(2 * time.Minute):
			fmt.Println("The app is still running; stopping it.")
			stop()
			<-exited
		}
	} else {
		select {
		case err := <-exited:
			if err != nil {
				return fatalCode("the engine run failed: " + err.Error())
			}
		case <-time.After(time.Duration(secs)*time.Second + 3*time.Minute):
			stop()
			return fatalCode("the engine run didn't finish")
		}
	}

	out, err := sudo("nft", "-j", "list", "table", "inet", nftTable)
	if err != nil {
		return fatalCode("reading the counters: " + err.Error())
	}
	flows, total, err := parseNft(out)
	if err != nil {
		return fatalCode("reading the counters: " + err.Error())
	}
	endpoints := endpointSet(endpoint, readEndpoints())
	if len(endpoints) == 0 {
		return fatalCode("the app didn't record a VPN connection during the run (did it connect?); pass -endpoint ip:port")
	}
	pol := policy{local: localAddrs(), endpoints: endpoints, remotePort: remotePort, remoteTailscale: remoteTailscale, engineRun: !app}
	pol.servers, pol.listen = appLocalServices()
	if dns != nil {
		pol.nameservers = map[string]bool{}
		for _, ns := range nameservers {
			pol.nameservers[ns] = true
		}
		pol.lookups = dns.lookups()
		n := dns.udpQueries()
		pol.recordedUDP = &n
	}
	return verdict(flows, total, pol, selftest)
}

// nftRules counts every packet sent by processes in cgroup (a path relative
// to /sys/fs/cgroup), per source, destination and ports, and redirects their
// DNS queries to the system's nameservers to dns (after counting them).
func nftRules(cgroup string, nameservers []string, dns *dnsLogger) string {
	level := strings.Count(cgroup, "/") + 1
	set := func(name, addr, rest string) string {
		return fmt.Sprintf("\tset %s { type %s . %s%s; flags dynamic; counter; size 65535; }\n", name, addr, addr, rest)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n", nftTable)
	b.WriteString("\tcounter total {}\n")
	b.WriteString(set("tcp4", "ipv4_addr", " . inet_service . inet_service"))
	b.WriteString(set("udp4", "ipv4_addr", " . inet_service . inet_service"))
	b.WriteString(set("tcp6", "ipv6_addr", " . inet_service . inet_service"))
	b.WriteString(set("udp6", "ipv6_addr", " . inet_service . inet_service"))
	b.WriteString(set("other4", "ipv4_addr", " . inet_proto"))
	b.WriteString(set("other6", "ipv6_addr", " . inet_proto"))
	fmt.Fprintf(&b, "\tchain out {\n\t\ttype filter hook output priority -400; policy accept;\n\t\tsocket cgroupv2 level %d %q goto watched\n\t}\n", level, cgroup)
	if dns != nil && len(nameservers) > 0 {
		u, t := dns.ports()
		set := strings.Join(nameservers, ", ")
		fmt.Fprintf(&b, "\tchain dns {\n\t\ttype nat hook output priority -100; policy accept;\n")
		fmt.Fprintf(&b, "\t\tsocket cgroupv2 level %d %q ip daddr { %s } udp dport 53 redirect to :%d\n", level, cgroup, set, u)
		fmt.Fprintf(&b, "\t\tsocket cgroupv2 level %d %q ip daddr { %s } tcp dport 53 redirect to :%d\n", level, cgroup, set, t)
		b.WriteString("\t}\n")
	}
	b.WriteString(`	chain watched {
		counter name "total"
		meta nfproto ipv4 meta l4proto tcp update @tcp4 { ip saddr . ip daddr . tcp sport . tcp dport } return
		meta nfproto ipv4 meta l4proto udp update @udp4 { ip saddr . ip daddr . udp sport . udp dport } return
		meta nfproto ipv6 meta l4proto tcp update @tcp6 { ip6 saddr . ip6 daddr . tcp sport . tcp dport } return
		meta nfproto ipv6 meta l4proto udp update @udp6 { ip6 saddr . ip6 daddr . udp sport . udp dport } return
		meta nfproto ipv4 update @other4 { ip saddr . ip daddr . meta l4proto } return
		meta nfproto ipv6 update @other6 { ip6 saddr . ip6 daddr . meta l4proto } return
	}
}
`)
	return b.String()
}

// parseNft reads the sets' elements and the total counter from
// `nft -j list table`.
func parseNft(data []byte) ([]flow, int64, error) {
	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, 0, err
	}
	var flows []flow
	var total int64 = -1
	for _, obj := range doc.Nftables {
		if raw, ok := obj["counter"]; ok {
			var c struct {
				Name    string `json:"name"`
				Packets int64  `json:"packets"`
			}
			json.Unmarshal(raw, &c)
			if c.Name == "total" {
				total = c.Packets
			}
			continue
		}
		raw, ok := obj["set"]
		if !ok {
			continue
		}
		var s struct {
			Name string `json:"name"`
			Elem []struct {
				Elem struct {
					Val struct {
						Concat []json.RawMessage `json:"concat"`
					} `json:"val"`
					Counter struct {
						Packets int64 `json:"packets"`
						Bytes   int64 `json:"bytes"`
					} `json:"counter"`
				} `json:"elem"`
			} `json:"elem"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, 0, fmt.Errorf("set: %w", err)
		}
		proto := strings.TrimRight(s.Name, "46")
		for _, e := range s.Elem {
			v := e.Elem.Val.Concat
			f := flow{proto: proto, packets: e.Elem.Counter.Packets, bytesSent: e.Elem.Counter.Bytes}
			if len(v) < 3 {
				return nil, 0, fmt.Errorf("set %s: unexpected element", s.Name)
			}
			json.Unmarshal(v[0], &f.src)
			json.Unmarshal(v[1], &f.dst)
			if proto == "other" {
				json.Unmarshal(v[2], &f.proto) // "icmp", "ipv6-icmp", …
				if f.proto == "" {
					var n int
					json.Unmarshal(v[2], &n)
					f.proto = "protocol " + strconv.Itoa(n)
				}
			} else if len(v) == 4 {
				json.Unmarshal(v[2], &f.sport)
				json.Unmarshal(v[3], &f.dport)
			}
			flows = append(flows, f)
		}
	}
	if total < 0 {
		return nil, 0, errors.New("no total counter")
	}
	return flows, total, nil
}

// scopeCgroup waits until systemd has moved pid into the new scope and
// returns its cgroup path relative to /sys/fs/cgroup.
func scopeCgroup(pid int, unit string) (string, error) {
	for i := 0; i < 100; i++ {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			return "", fmt.Errorf("the watched process exited at once: %w", err)
		}
		p := strings.TrimSpace(strings.TrimPrefix(string(b), "0::"))
		if strings.HasSuffix(p, "/"+unit+".scope") {
			return strings.TrimPrefix(p, "/"), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", errors.New("systemd didn't create the scope for the watched process")
}

func appRunning() bool {
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		// The package's /usr/bin/sharkbitetorrent shows as "sharkbitetorren".
		if b, err := os.ReadFile("/proc/" + e.Name() + "/comm"); err == nil && strings.EqualFold(strings.TrimSpace(string(b)), linuxAppComm) {
			return true
		}
	}
	return false
}

// staleKernel explains the usual reason the counters can't be installed:
// the kernel was updated since the last reboot, so the running kernel's
// modules (nft_socket, nft_redirect) are gone from /lib/modules.
func staleKernel() string {
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return ""
	}
	release := unix.ByteSliceToString(u.Release[:])
	if _, err := os.Stat(filepath.Join("/lib/modules", release)); err == nil {
		return ""
	}
	return "the kernel was updated since this computer last started (it runs " + release + ", whose modules are no longer installed), so the firewall counters can't be loaded. Restart the computer, then run the check again"
}
