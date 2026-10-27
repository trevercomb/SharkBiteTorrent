//go:build darwin

package main

// macOS: tcpdump's pktap interface (-i pktap,all -k NPD) tags every packet,
// on every interface, with the process that sent it ("proc") and its
// direction. The packets are read live and counted per address, protocol
// and ports; nothing is written to disk.
//
// pktap leaves some packets untagged, often a connection's outgoing ones
// while tagging its replies. A packet therefore counts as the app's if it's
// tagged with a watched process, or if it's untagged (or tagged with the
// kernel) and either another packet of its connection was tagged with one,
// or it was sent from a port a watched process had open (lsof, polled once
// a second). Untagged TCP/UDP still unaccounted for fails the run, unless
// -allow-untagged is given after looking at it: it may be any program's.
//
// System DNS: lookups go to mDNSResponder, which sends the packets itself.
// pktap's tag for the process they're for ("eproc") proved unreliable, so
// lookups are judged from mDNSResponder's log instead: every request is
// logged with the client's PID, and a hash of the name (the name itself is
// hidden). The check learns the hashes of the names that are allowed (a VPN
// account's documented direct calls, and localhost) by looking them up
// itself first; any other lookup by a watched process is a leak, named if
// its hash is known. Like the Windows check, a lookup counts even when it's
// answered from the cache. mDNSResponder's own packets aren't judged.
//
// A VPN account's documented direct calls are only allowed from the app's
// main process, while it connects (within 90 s before a WireGuard handshake
// that starts a session, or 15 s after), and HTTPS only to the addresses DNS
// answered for their names. A VPN server the app recorded only counts if a
// WireGuard handshake initiation was sent to it.
//
// Watched processes: the app (or the engine run, a child process), anything
// it starts, and WebKit's helpers working for it. Those are started by
// launchd, not the app, so they're found by asking launchd which app is
// responsible for them (launchctl procinfo); -app opens the app through
// LaunchServices so that's the app, not the terminal. -app fails unless
// every WebKit process could be asked and the app's Networking one was
// found. Process lists are polled once a second, so a child that starts,
// sends and exits within that is seen only through pktap's tags.
//
// Not seen: work system services do for the app in their own name. The
// one that matters is certificate checking: macOS checks TLS certificates
// in trustd, which may fetch revocation data or missing certificates
// itself. The app verifies certificates in Go with its own roots on macOS,
// so it shouldn't cause any; the check lists what trustd and the like sent
// during the run, which may have been for any program.
//
// It runs as the logged-in user and uses sudo (asked once) for tcpdump, log
// stream, launchctl procinfo and flushing the DNS cache.
//
// Allowed otherwise as on Linux (see classify): this computer except DNS,
// UDP to the VPN server, the user's local search server, the search proxy
// answering Prowlarr, and the remote control's replies to the home network
// or Tailscale.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"sharkbite/internal/systool"
	"sharkbite/internal/warp"
)

const darwinExe = "/Applications/SharkBiteTorrent.app/Contents/MacOS/SharkBiteTorrent"

var allowUntagged = flag.Bool("allow-untagged", false, "macOS: pass even if some TCP/UDP couldn't be attributed to any process (look at the list first)")

// plantedNSLookup is the selftest's lookup through Network.framework (the
// API WebKit uses), made by nscurl as a watched process.
const plantedNSLookup = "example.net"

func runDarwin(secs int, selftest, app bool, exe, endpointFlag, childWarp string) int {
	if childWarp != "" {
		return runChild(childWarp, secs, selftest)
	}
	if exe == defaultExe() {
		exe = darwinExe
	}
	if strings.HasSuffix(strings.TrimSuffix(exe, "/"), ".app") {
		exe = filepath.Join(exe, "Contents", "MacOS", "SharkBiteTorrent")
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
		if darwinAppRunning() {
			return fatalCode("quit Shark BiteTorrent first (Cmd+Q): this check launches it itself, to cover its startup")
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
	} else {
		if len(flag.Args()) != 1 {
			return fatalCode("usage: leakcheck [-selftest] [-seconds N] <magnet|file.torrent>   or   leakcheck -app")
		}
		// A throwaway WARP account, registered here, by this process, which
		// isn't watched: registering is a direct API call by design.
		tmp, err := os.MkdirTemp("", "leakcheck")
		if err != nil {
			return fatalCode(err.Error())
		}
		defer os.RemoveAll(tmp)
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

	fmt.Println("YOU: if sudo asks for your password, type it (it's needed for tcpdump, the DNS log, launchctl and flushing the DNS cache).")
	if err := sudoInteractive("-v"); err != nil {
		return fatalCode("sudo: " + err.Error())
	}
	keepAlive := time.NewTicker(60 * time.Second)
	defer keepAlive.Stop()
	go func() {
		for range keepAlive.C {
			sudo("-v")
		}
	}()
	// So the documented hosts' lookups go out and their answers are seen.
	sudo("dscacheutil", "-flushcache")
	sudo("killall", "-HUP", "mDNSResponder")

	capture, err := startPktap()
	if err != nil {
		return fatalCode("starting tcpdump: " + err.Error())
	}
	defer capture.stop()
	dnsLog, err := startMDNSLog()
	if err != nil {
		return fatalCode("starting the DNS log: " + err.Error())
	}
	defer dnsLog.stop()

	ownPID := os.Getpid()
	controls := &controls{}
	controls.run() // at the start; again at the end
	learned := learnNames()

	// The app is opened the way Finder opens it (LaunchServices), not as a
	// child of this terminal: macOS then makes the app, not the terminal,
	// responsible for its WebKit helpers, which is how they're found. -W
	// makes open wait until the app quits.
	var cmd *exec.Cmd
	var appLog *os.File
	if app {
		bundle := strings.TrimSuffix(exe, "/Contents/MacOS/SharkBiteTorrent")
		if appLog, err = os.CreateTemp("", "leakcheck-app-*.log"); err != nil {
			return fatalCode(err.Error())
		}
		appLog.Close()
		defer os.Remove(appLog.Name())
		cmd = exec.Command("open", "-n", "-W", "--stdout", appLog.Name(), "--stderr", appLog.Name(), bundle)
	} else {
		cmd = exec.Command(childArgs[0], childArgs[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // Ctrl+C reaches only us; we stop it
	}
	if err := cmd.Start(); err != nil {
		return fatalCode("starting the watched process: " + err.Error())
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	root := cmd.Process.Pid
	if app {
		for i := 0; i < 60 && (root == cmd.Process.Pid); i++ {
			time.Sleep(500 * time.Millisecond)
			if out, err := systool.Command("pgrep", "-x", "SharkBiteTorrent").Output(); err == nil {
				if p, err := strconv.Atoi(strings.Fields(string(out))[0]); err == nil {
					root = p
				}
			}
		}
		if root == cmd.Process.Pid {
			cmd.Process.Kill()
			return fatalCode("the app didn't start")
		}
	}
	stop := func() { // a last resort (Ctrl+C, or the app not quit in time): at once, nothing more to watch
		if app {
			syscall.Kill(root, syscall.SIGKILL)
		} else {
			syscall.Kill(-root, syscall.SIGKILL)
		}
	}
	watched := newMacWatch(root)
	tracking := make(chan struct{})
	trackingDone := make(chan struct{})
	go func() {
		defer close(trackingDone)
		for {
			watched.refresh()
			select {
			case <-tracking:
				watched.refresh() // once more, after everything stopped
				return
			case <-time.After(time.Second):
			}
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		fmt.Println("\ninterrupted")
		stop()
		capture.stop()
		dnsLog.stop()
		stopEndpointRecording()
		os.Exit(130)
	}()
	start := time.Now()

	// The selftest's lookup through Network.framework, by a watched process.
	nsPID := 0
	if selftest {
		if nsPID, err = plantNSLookup(watched); err != nil {
			fmt.Println("selftest: couldn't run nscurl for the Network.framework lookup:", err)
		}
	}

	if app {
		fmt.Printf("Started Shark BiteTorrent (PID %d); watching it for %d seconds.\nYOU: leave the window alone for the first minute (that covers startup). Then: search and add a result; type or paste in the magnet box; open every Settings section (type in a text field, click through VPN providers without saving); turn the search proxy on and click Check it's working (then off again if you don't use it); switch to another VPN provider and save, then back (that covers reconnecting); click Check for updates; and paste https://example.com/ in Paste links (its \"getting downloads\" window opens: let the page load, then close it).\n", root, secs)
		select {
		case err := <-exited:
			out, _ := os.ReadFile(appLog.Name())
			return fatalCode(fmt.Sprintf("the app exited after %ds (%v): %s", int(time.Since(start).Seconds()), err, lastLines(string(out), 10)))
		case <-time.After(time.Duration(secs) * time.Second):
		}
		// Quitting is covered too.
		fmt.Println("\nYOU: now quit Shark BiteTorrent (Cmd+Q).")
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
	close(tracking)
	<-trackingDone
	controls.run()
	time.Sleep(3 * time.Second) // let the last packets and log lines arrive
	res, err := capture.finish()
	if err != nil {
		return fatalCode("reading tcpdump's output: " + err.Error())
	}
	reqs, logNotices := dnsLog.finish()

	fmt.Printf("\nwatched %d processes (%d WebKit helpers); tcpdump saw %d packets; DNS log: %d lookups\n", watched.len(), watched.webkitCount(), res.packets, len(reqs))
	if res.withDir == 0 {
		fmt.Println("FAIL: tcpdump didn't print packet directions (-k D), so what the app sent couldn't be told from what it received")
		return 1
	}
	if res.dropped != 0 {
		fmt.Printf("FAIL: tcpdump dropped %d packets, so not everything was checked (try fewer -seconds or a smaller torrent)\n", res.dropped)
		return 1
	}
	if res.unparsed > 0 {
		fmt.Printf("FAIL: %d IP packets in tcpdump's output couldn't be read, so they weren't checked; e.g.:\n", res.unparsed)
		for _, l := range res.unparsedSample {
			fmt.Println("  ", l)
		}
		return 1
	}
	if len(logNotices) > 0 {
		fmt.Println("FAIL: the DNS log reported losing messages, so lookups may have been missed:")
		for _, l := range logNotices {
			fmt.Println("  ", l)
		}
		return 1
	}
	if missing := controls.missing(res, reqs, ownPID); len(missing) > 0 {
		fmt.Println("FAIL: the check's own test traffic wasn't all recorded (" + strings.Join(missing, ", ") + "), so the run wasn't fully covered")
		return 1
	}
	if app {
		if bad := watched.unresolved(res.sentOffMachine); len(bad) > 0 {
			fmt.Println("FAIL: launchctl couldn't say which app these WebKit processes work for, so their traffic may not have been covered:", strings.Join(bad, ", "))
			return 1
		}
		if !watched.hasNetworkingHelper() {
			fmt.Println("FAIL: the app's WebKit Networking process wasn't found (launchctl procinfo), so its web view's traffic wasn't covered")
			return 1
		}
	}

	// The VPN servers: those the app recorded, if it sent them a WireGuard
	// handshake (the check doesn't take the app's word for them).
	var endpoints = map[string]bool{}
	for ep := range endpointSet(endpoint, readEndpoints()) {
		if res.handshakeFrom(ep, watched) {
			endpoints[ep] = true
		} else {
			fmt.Println("note: no WireGuard handshake was sent to", ep, "(recorded as a VPN server), so it doesn't count as one")
		}
	}
	if len(endpoints) == 0 {
		return fatalCode("no WireGuard handshake to a VPN server the app recorded was seen (did it connect?); pass -endpoint ip:port")
	}
	local := watched.localAddrs()
	res.claimUnseenChildren(watched, local)
	windows := res.connectWindows(endpoints, watched)
	flows, un := res.forWatched(watched, local, mdnsPIDs(reqs), root, windows)
	fmt.Printf("untagged packets: %d claimed through their connection or port; unclaimed: %d ICMP (the kernel's), %d DNS (judged from the log), %d network-drive (the kernel's, home network), %d TCP/UDP\n",
		un.rescued, un.icmp, un.dns, un.fileSharing, un.unclaimed)
	res.showSystemActivity(reqs)
	var total int64
	for _, f := range flows {
		total += f.packets
	}
	pol := policy{local: local, endpoints: endpoints, remotePort: remotePort, remoteTailscale: remoteTailscale, engineRun: !app,
		lookups: watchedLookups(reqs, watched, ownPID, root, windows, learned, res.answersByName())}
	pol.servers, pol.listen = appLocalServices()
	if selftest {
		res.showPlanted(watched)
		if nsPID == 0 || !clientLogged(reqs, nsPID) {
			fmt.Println("selftest FAILED: the Network.framework lookup (nscurl, as WebKit would make it) wasn't found in the DNS log, so the check can't see lookups made that way")
			return 1
		}
	}
	if len(un.flows) > 0 {
		fmt.Println("TCP/UDP that pktap tagged with no process, on connections and ports no watched process had (whose it was can't be told):")
		for _, f := range un.flows {
			fmt.Println("  ", f)
		}
		if !*allowUntagged && !selftest {
			fmt.Println("FAIL: that traffic may have been the app's. If you're sure it wasn't (another program's), run again with -allow-untagged.")
			return 1
		}
	}
	return verdict(flows, total, pol, selftest)
}

// plantNSLookup runs nscurl (Network.framework, like WebKit) against
// plantedNSLookup as a watched process and returns its PID.
func plantNSLookup(w *macWatch) (int, error) {
	cmd := exec.Command("/usr/bin/nscurl", "https://"+plantedNSLookup+"/")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	w.add(cmd.Process.Pid)
	go func() {
		t := time.AfterFunc(20*time.Second, func() { cmd.Process.Kill() })
		cmd.Wait()
		t.Stop()
	}()
	fmt.Println("selftest: planted a Network.framework lookup of", plantedNSLookup, "(nscurl, PID", strconv.Itoa(cmd.Process.Pid)+")")
	return cmd.Process.Pid, nil
}

func clientLogged(reqs []mdnsRequest, pid int) bool {
	for _, r := range reqs {
		if r.client == pid {
			return true
		}
	}
	return false
}

// showPlanted prints every packet tcpdump saw to or from the selftest's
// planted connection, with its tags, so a missed one can be explained.
func (r *pktapResult) showPlanted(w *macWatch) {
	host, _, _ := net.SplitHostPort(plantedConn)
	for k, s := range r.counts {
		if k.dst == host || k.src == host {
			fmt.Printf("  planted connection's packets: %s %s:%d > %s:%d proc %d eproc %d dir %q watched %v: %d\n",
				k.proto, k.src, k.sport, k.dst, k.dport, k.proc, k.eproc, k.dir, w.has(k.proc) || w.has(k.eproc), s.n)
		}
	}
}

// systemServices check certificates, signatures and the like for other
// programs, in their own name (see the top of this file).
var systemServices = []string{"trustd", "ocspd", "securityd", "syspolicyd", "nsurlsessiond"}

// showSystemActivity lists what those services sent to other computers,
// and how many lookups they made, during the run.
func (r *pktapResult) showSystemActivity(reqs []mdnsRequest) {
	for _, name := range systemServices {
		var packets int64
		dests := map[string]bool{}
		for k, s := range r.counts {
			if k.dir == "in" || r.procNames[k.proc] != name || k.proto == "icmp" || k.proto == "ipv6-icmp" {
				continue
			}
			packets += s.n
			dests[net.JoinHostPort(k.dst, strconv.Itoa(k.dport))] = true
		}
		lookups := 0
		for _, q := range reqs {
			if clientName(r, q.client) == name {
				lookups++
			}
		}
		if packets > 0 || lookups > 0 {
			var d []string
			for x := range dests {
				d = append(d, x)
			}
			sort.Strings(d)
			sent := "no packets"
			if packets > 0 {
				sent = fmt.Sprintf("%d packets to %s", packets, strings.Join(d, ", "))
			}
			fmt.Printf("note: system activity (for any program, possibly the app): %s sent %s, and made %d lookups\n", name, sent, lookups)
		}
	}
}

var clientNames = map[int]string{}

// clientName is a lookup client's process name: from pktap's tags, or ps.
func clientName(r *pktapResult, pid int) string {
	if n := r.procNames[pid]; n != "" {
		return n
	}
	if n, ok := clientNames[pid]; ok {
		return n
	}
	clientNames[pid] = pidName(pid)
	return clientNames[pid]
}

func pidName(pid int) string {
	out, err := systool.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(string(out)))
}

func darwinAppRunning() bool {
	return systool.Command("pgrep", "-x", "SharkBiteTorrent").Run() == nil
}

// timedLookup is a lookup this process made, and when, so its requests can
// be found in mDNSResponder's log (which hides the name).
type timedLookup struct {
	name     string
	from, to time.Time
}

// lookupFor looks name up from this process, alone, and returns when.
func lookupFor(name string, d time.Duration) timedLookup {
	time.Sleep(200 * time.Millisecond) // keep it apart from the previous one
	t := timedLookup{name: name, from: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	(&net.Resolver{}).LookupHost(ctx, name)
	cancel()
	t.to = time.Now()
	return t
}

// learnNames looks up, from this process, every name a watched process may
// look up by design (and the selftest's planted ones), so their hashes in
// mDNSResponder's log can be named.
func learnNames() []timedLookup {
	var out []timedLookup
	for _, n := range append([]string{"localhost", plantedLookup, plantedNSLookup}, sortedKeys(documentedDirect)...) {
		out = append(out, lookupFor(n, 5*time.Second))
	}
	return out
}

// --- the processes being watched ---

type macWatch struct {
	mu        sync.Mutex
	pids      map[int]bool
	asked     map[int]bool    // WebKit processes launchctl answered for
	resp      map[int]int     // the process launchd says each of those is for
	failures  map[int]int     // WebKit processes launchctl didn't answer for, how often
	webkit    map[int]string  // the app's WebKit helpers: PID → name
	ports     map[string]int  // "tcp:50000", "udp:58017": ports watched processes had open → PID
	addrs     map[string]bool // this computer's addresses, seen during the run
	webkitAll map[int]string  // every WebKit process seen, to report unresolved ones
	gone      map[int]bool    // WebKit processes that exited before launchctl answered
	seen      map[int]bool    // every process seen running during the run
}

func newMacWatch(root int) *macWatch {
	return &macWatch{pids: map[int]bool{root: true}, asked: map[int]bool{}, resp: map[int]int{}, failures: map[int]int{},
		webkit: map[int]string{}, ports: map[string]int{}, addrs: localAddrs(), webkitAll: map[int]string{},
		gone: map[int]bool{}, seen: map[int]bool{}}
}

func (w *macWatch) add(pid int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pids[pid] = true
}

func (w *macWatch) has(pid int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pids[pid]
}

func (w *macWatch) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pids)
}

func (w *macWatch) webkitCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.webkit)
}

// portPID is the watched process that had proto/port open (0 if none).
func (w *macWatch) portPID(proto string, port int) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ports[proto+":"+strconv.Itoa(port)]
}

func (w *macWatch) localAddrs() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := map[string]bool{}
	for a := range w.addrs {
		m[a] = true
	}
	return m
}

func (w *macWatch) hasNetworkingHelper() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, name := range w.webkit {
		if strings.HasSuffix(name, "com.apple.WebKit.Networking") {
			return true
		}
	}
	return false
}

// unresolved is the WebKit processes launchctl never answered for that may
// have been the app's and done something: still running, or gone after
// sending packets (sent reports that).
func (w *macWatch) unresolved(sent func(pid int) bool) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for p, name := range w.webkitAll {
		if !w.asked[p] && (!w.gone[p] || sent(p)) {
			out = append(out, fmt.Sprintf("%s (PID %d)", filepath.Base(name), p))
		}
	}
	sort.Strings(out)
	return out
}

// everSeen reports whether pid was seen running during the run.
func (w *macWatch) everSeen(pid int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen[pid]
}

var responsiblePID = regexp.MustCompile(`(?m)^\s*responsible pid = (\d+)`)

// refresh adds the watched processes' descendants and the WebKit processes
// launchd says the app is responsible for, and notes the ports they have
// open and this computer's addresses.
func (w *macWatch) refresh() {
	out, err := systool.Command("ps", "-A", "-o", "pid=,ppid=,comm=").Output()
	if err != nil {
		return
	}
	children := map[int][]int{}
	alive := map[int]bool{}
	webkit := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		children[ppid] = append(children[ppid], pid)
		alive[pid] = true
		w.mu.Lock()
		w.seen[pid] = true
		w.mu.Unlock()
		if name := strings.Join(f[2:], " "); strings.Contains(name, "com.apple.WebKit.") {
			webkit[pid] = name
		}
	}
	w.mu.Lock()
	var ask []int
	for p, name := range webkit {
		if !w.asked[p] && w.failures[p] < 5 {
			ask = append(ask, p)
		}
		w.webkitAll[p] = name
	}
	w.mu.Unlock()
	for _, p := range ask {
		info, err := sudo("launchctl", "procinfo", strconv.Itoa(p))
		m := responsiblePID.FindSubmatch(info)
		w.mu.Lock()
		if m == nil {
			if !alive[p] || err != nil && !processExists(p) {
				w.gone[p], w.failures[p] = true, 5 // exited first: whose it was can't be asked now
			} else {
				w.failures[p]++ // asked again next time
			}
		} else {
			w.asked[p] = true
			w.resp[p], _ = strconv.Atoi(string(m[1]))
		}
		w.mu.Unlock()
	}
	w.mu.Lock()
	// Descendants, and WebKit processes for any watched process, until
	// nothing new turns up: the browser window's process (a child) may be
	// found in the same pass as the WebKit processes that are its.
	for changed := true; changed; {
		changed = false
		for p := range w.pids {
			for _, c := range children[p] {
				if !w.pids[c] {
					w.pids[c], changed = true, true
				}
			}
		}
		for p, r := range w.resp {
			if w.pids[r] && !w.pids[p] {
				w.pids[p], w.webkit[p], changed = true, w.webkitAll[p], true
			}
		}
	}
	var pids []string
	for p := range w.pids {
		if alive[p] {
			pids = append(pids, strconv.Itoa(p))
		}
	}
	for a := range localAddrs() {
		w.addrs[a] = true
	}
	w.mu.Unlock()
	if len(pids) > 0 {
		// lsof shows sockets, connected or not (unconnected UDP as *:port).
		out, _ := systool.Command("lsof", "-nP", "-i", "-a", "-p", strings.Join(pids, ",")).Output()
		ports := socketPorts(string(out))
		w.mu.Lock()
		for p, pid := range ports {
			w.ports[p] = pid
		}
		w.mu.Unlock()
	}
}

func processExists(pid int) bool {
	return syscall.Kill(pid, 0) == nil || errors.Is(syscall.Kill(pid, 0), syscall.EPERM)
}

// lsofPort is the local port in lsof's NAME column: "127.0.0.1:49602",
// "*:58017", "[::1]:50736->[::1]:9696".
var lsofPort = regexp.MustCompile(`:(\d+)(?:->|\s|$)`)

// socketPorts reads `lsof -nP -i` output into "tcp:PORT"/"udp:PORT" → PID.
func socketPorts(out string) map[string]int {
	m := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 9 || (f[7] != "TCP" && f[7] != "UDP") {
			continue
		}
		local := strings.SplitN(f[8], "->", 2)[0]
		if pm := lsofPort.FindStringSubmatch(local + " "); pm != nil {
			pid, _ := strconv.Atoi(f[1])
			m[strings.ToLower(f[7])+":"+pm[1]] = pid
		}
	}
	return m
}

// --- a privileged process whose output is read line by line ---

type lineReader struct {
	cmd    *exec.Cmd
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	stderr bytes.Buffer
}

// startLines runs sudo args, passing each stdout line to onLine and
// returning once a line (stdout or stderr) satisfies ready.
func startLines(args []string, onLine func(string), ready func(string) bool) (*lineReader, error) {
	l := &lineReader{done: make(chan struct{})}
	l.cmd = systool.Command("sudo", append([]string{"-n"}, args...)...)
	out, err := l.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errPipe, err := l.cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := l.cmd.Start(); err != nil {
		return nil, err
	}
	isReady := make(chan bool, 2)
	var readyOnce sync.Once
	signalReady := func(ok bool) { readyOnce.Do(func() { isReady <- ok }) }
	go func() {
		sc := bufio.NewScanner(errPipe)
		for sc.Scan() {
			line := sc.Text()
			l.mu.Lock()
			l.stderr.WriteString(line + "\n")
			l.mu.Unlock()
			if ready(line) {
				signalReady(true)
			}
		}
		signalReady(false)
	}()
	go func() {
		defer close(l.done)
		r := bufio.NewReaderSize(out, 1<<20)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				line = strings.TrimRight(line, "\r\n")
				if ready(line) {
					signalReady(true)
				}
				onLine(line)
			}
			if err != nil {
				if err != io.EOF {
					fmt.Fprintln(os.Stderr, "leakcheck: reading", args[0]+":", err)
				}
				signalReady(false)
				return
			}
		}
	}()
	select {
	case ok := <-isReady:
		if !ok {
			return nil, errors.New(strings.TrimSpace(l.stderrText()))
		}
	case <-time.After(15 * time.Second):
		l.stop()
		return nil, errors.New(args[0] + " didn't start")
	}
	return l, nil
}

func (l *lineReader) stderrText() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stderr.String()
}

// stop ends the process (safe to call more than once).
func (l *lineReader) stop() {
	l.once.Do(func() {
		if l.cmd.Process == nil {
			return
		}
		// sudo passes the signal on (tcpdump then prints its counts).
		l.cmd.Process.Signal(os.Interrupt)
		ch := make(chan struct{})
		go func() { l.cmd.Wait(); close(ch) }()
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			l.cmd.Process.Kill()
			<-ch
		}
	})
}

// --- tcpdump ---

type pktap struct {
	*lineReader
	mu  sync.Mutex
	res *pktapResult
}

// pktapKey is one counted set of packets: who sent (or received) them
// (proc) and for whom (eproc), their direction, addresses, protocol and
// ports.
type pktapKey struct {
	proc, eproc  int
	dir          string // "in" or "out"
	proto        string
	src, dst     string
	sport, dport int
}

// handshake is a WireGuard handshake initiation sent: when, and by whom
// (Tailscale sends them too, to its own peers).
type handshake struct {
	at time.Time
	sender
}

// sender is who sent a packet: its tag, and its source port (for untagged
// ones, matched against the ports watched processes had open).
type sender struct {
	proc  int
	sport int
}

// ours reports whether a watched process sent it.
func (s sender) ours(w *macWatch) bool {
	return w.has(s.proc) || (s.proc == 0 || s.proc == kernelPID) && w.portPID("udp", s.sport) != 0
}

// pktapStat is how many packets a key had, and when the first and last were.
type pktapStat struct {
	n           int64
	first, last time.Time
}

type pktapResult struct {
	packets, dropped int64
	withDir          int64 // packets whose direction tcpdump printed
	counts           map[pktapKey]*pktapStat
	procNames        map[int]string      // PID → process name, from the tags
	queryNames       map[string]string   // server|id|client port → name, every DNS query seen
	answers          map[string][]string // the same key → addresses
	// handshakes: the times WireGuard handshake initiations (148-byte UDP)
	// were sent to each ip:port; tunnelSecs: the seconds UDP was sent there,
	// by each sender.
	handshakes     map[string][]handshake
	tunnelSecs     map[string]map[sender]map[int64]bool
	unparsed       int64 // IP packets whose line couldn't be read
	unparsedSample []string
	day            time.Time // the run's date, for tcpdump's times of day
}

func newPktapResult() *pktapResult {
	now := time.Now()
	return &pktapResult{counts: map[pktapKey]*pktapStat{}, procNames: map[int]string{}, queryNames: map[string]string{},
		answers: map[string][]string{}, handshakes: map[string][]handshake{}, tunnelSecs: map[string]map[sender]map[int64]bool{},
		day: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)}
}

func startPktap() (*pktap, error) {
	// -l: a line at a time; -nn: numeric addresses and ports; -B: a 64 MB
	// buffer so a busy download doesn't make the kernel drop packets.
	p := &pktap{res: newPktapResult()}
	lr, err := startLines([]string{"tcpdump", "-i", "pktap,all", "-k", "NPD", "-nn", "-l", "-B", "65536", "-s", "600"},
		func(line string) {
			p.mu.Lock()
			p.res.add(line)
			p.mu.Unlock()
		},
		func(line string) bool { return strings.HasPrefix(line, "listening on") })
	if err != nil {
		return nil, err
	}
	p.lineReader = lr
	return p, nil
}

var (
	capturedRe = regexp.MustCompile(`(\d+) packets? captured`)
	droppedRe  = regexp.MustCompile(`(\d+) packets? dropped by kernel`)
)

// finish stops tcpdump and returns what it saw.
func (p *pktap) finish() (*pktapResult, error) {
	p.stop()
	<-p.done
	errText := p.stderrText()
	m := capturedRe.FindStringSubmatch(errText)
	d := droppedRe.FindStringSubmatch(errText)
	if m == nil || d == nil {
		return nil, errors.New("tcpdump didn't report its packet counts: " + lastLines(errText, 5))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.res.packets, _ = strconv.ParseInt(m[1], 10, 64)
	p.res.dropped, _ = strconv.ParseInt(d[1], 10, 64)
	return p.res, nil
}

// kernelPID stands for kernel_task, whose real PID (0) means "untagged" here.
const kernelPID = -1

var (
	// 00:59:12.522988 (proc mDNSResponder:656, eproc dscacheutil:17455, out) IP 100.72.141.16.53153 > 100.100.100.100.53: 48106+ AAAA? example.com. (46)
	pktapLine = regexp.MustCompile(`^(\d\d:\d\d:\d\d\.\d+) \((.*?)\) (IP6?) (\S+) > (\S+?): (.*)$`)
	ipLine    = regexp.MustCompile(`^\S+ (?:\(.*?\) )?IP6? `)
	procRe    = regexp.MustCompile(`(?:^|, )proc (.*?):(\d+)`)
	eprocRe   = regexp.MustCompile(`(?:^|, )eproc (.*?):(\d+)`)
	dirRe     = regexp.MustCompile(`(?:^|, )(in|out)$`)                                                   // "(in)", "(proc x:1, out)"
	dnsQ      = regexp.MustCompile(`(?:^|: )(\d+)\+?(?: \[[^\]]*\])? ([A-Za-z0-9]+)\? (\S+) \(\d+\)\s*$`) // "Type65?" too
	dnsR      = regexp.MustCompile(`(?:^|: )(\d+)[*|$-]* \d+/\d+/\d+(?: (.*?))? \(\d+\)\s*$`)
	dnsAddr   = regexp.MustCompile(`\b(?:A|AAAA) ([0-9A-Fa-f:.]+)`)
)

// add counts one line of tcpdump's output.
func (r *pktapResult) add(line string) {
	line = strings.TrimRight(line, "\r\n")
	m := pktapLine.FindStringSubmatch(line)
	if m == nil {
		if ipLine.MatchString(line) {
			r.unparsed++ // an IP packet we can't read: not checked, so the run fails
			if len(r.unparsedSample) < 5 {
				r.unparsedSample = append(r.unparsedSample, line)
			}
		}
		return // ARP, a continuation line, …
	}
	at := r.timeOf(m[1])
	k := pktapKey{}
	if pm := procRe.FindStringSubmatch(m[2]); pm != nil {
		k.proc, _ = strconv.Atoi(pm[2])
		if k.proc == 0 && pm[1] != "" {
			k.proc = kernelPID // "kernel_task:0": tagged, with the kernel
		}
		if pm[1] != "" {
			r.procNames[k.proc] = pm[1]
		}
	}
	if em := eprocRe.FindStringSubmatch(m[2]); em != nil {
		k.eproc, _ = strconv.Atoi(em[2])
	}
	if dm := dirRe.FindStringSubmatch(m[2]); dm != nil {
		k.dir = dm[1]
		r.withDir++
	}
	var hasPorts bool
	k.src, k.sport, hasPorts = splitPktapAddr(m[4], m[3] == "IP6")
	k.dst, k.dport, _ = splitPktapAddr(m[5], m[3] == "IP6")
	rest := m[6]
	switch {
	case strings.Contains(rest, "Flags ["):
		k.proto = "tcp"
	case strings.HasPrefix(rest, "ICMP6"):
		k.proto, k.sport, k.dport = "ipv6-icmp", 0, 0
	case strings.HasPrefix(rest, "ICMP"):
		k.proto, k.sport, k.dport = "icmp", 0, 0
	case hasPorts:
		k.proto = "udp"
	default:
		k.proto = "other"
	}
	s := r.counts[k]
	if s == nil {
		s = &pktapStat{first: at}
		r.counts[k] = s
	}
	s.n++
	s.last = at
	if k.proto == "udp" && k.dir != "in" {
		to := net.JoinHostPort(k.dst, strconv.Itoa(k.dport))
		if strings.HasSuffix(rest, "UDP, length 148") {
			r.handshakes[to] = append(r.handshakes[to], handshake{at, sender{k.proc, k.sport}})
		}
		who := sender{k.proc, k.sport}
		if r.tunnelSecs[to] == nil {
			r.tunnelSecs[to] = map[sender]map[int64]bool{}
		}
		if r.tunnelSecs[to][who] == nil {
			r.tunnelSecs[to][who] = map[int64]bool{}
		}
		r.tunnelSecs[to][who][at.Unix()] = true
	}
	if q := dnsQ.FindStringSubmatch(rest); q != nil && k.dport == 53 {
		r.queryNames[k.dst+"|"+q[1]+"|"+strconv.Itoa(k.sport)] = strings.ToLower(strings.TrimSuffix(q[3], "."))
	} else if a := dnsR.FindStringSubmatch(rest); a != nil && k.sport == 53 {
		key := k.src + "|" + a[1] + "|" + strconv.Itoa(k.dport)
		for _, am := range dnsAddr.FindAllStringSubmatch(a[2], -1) {
			r.answers[key] = append(r.answers[key], strings.TrimSuffix(am[1], ","))
		}
	}
}

// timeOf turns tcpdump's time of day into a time on the run's date (a run
// that passes midnight continues on the next day).
func (r *pktapResult) timeOf(hms string) time.Time {
	t, err := time.ParseInLocation("15:04:05.000000", hms, time.Local)
	if err != nil {
		return time.Time{}
	}
	at := r.day.Add(time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second + time.Duration(t.Nanosecond()))
	if at.Before(time.Now().Add(-12 * time.Hour)) {
		at = at.Add(24 * time.Hour)
	}
	return at
}

// answersByName is the addresses DNS returned for each name during the run,
// whoever asked.
func (r *pktapResult) answersByName() map[string][]string {
	out := map[string][]string{}
	for key, name := range r.queryNames {
		out[name] = append(out[name], r.answers[key]...)
	}
	return out
}

// window is a span of time, when the app was connecting to a VPN server.
type window struct{ from, to time.Time }

// handshakeFrom reports whether a watched process sent a WireGuard
// handshake initiation to ep (tagged with it, or untagged from its port).
func (r *pktapResult) handshakeFrom(ep string, w *macWatch) bool {
	for _, h := range r.handshakes[ep] {
		if h.ours(w) {
			return true
		}
	}
	return false
}

// connectWindows are the times the app may make a VPN account's documented
// direct calls: from 90 s before each handshake of its own that starts a
// session (one with no tunnel traffic of its own to that server in the 30 s
// before; later ones are WireGuard rekeying) to 15 s after. Another
// WireGuard client's handshakes to the same server (Cloudflare's own WARP
// app) don't open one.
func (r *pktapResult) connectWindows(endpoints map[string]bool, w *macWatch) []window {
	var out []window
	for ep := range endpoints {
		for _, h := range r.handshakes[ep] {
			if !h.ours(w) {
				continue
			}
			t := h.at
			busy := false
			for who, secs := range r.tunnelSecs[ep] {
				if !who.ours(w) {
					continue
				}
				for s := t.Unix() - 30; s < t.Unix(); s++ {
					if secs[s] {
						busy = true
					}
				}
			}
			if !busy {
				out = append(out, window{t.Add(-90 * time.Second), t.Add(15 * time.Second)})
			}
		}
	}
	return out
}

func within(ws []window, from, to time.Time) bool {
	for _, w := range ws {
		if !from.Before(w.from) && !to.After(w.to) {
			return true
		}
	}
	return false
}

// splitPktapAddr splits tcpdump's "1.2.3.4.443" or "2600::1.443" (the port
// after the last dot) into address and port; ICMP and the like have none.
func splitPktapAddr(s string, v6 bool) (addr string, port int, ok bool) {
	s = strings.TrimSuffix(s, ":")
	if i := strings.Index(s, "%"); i >= 0 && v6 { // fe80::1%en0.5353
		if j := strings.LastIndex(s, "."); j > i {
			s = s[:i] + s[j:]
		} else {
			s = s[:i]
		}
	}
	i := strings.LastIndex(s, ".")
	if v6 {
		if i < 0 || i < strings.LastIndex(s, ":") {
			return s, 0, false
		}
	} else if strings.Count(s, ".") != 4 {
		return s, 0, false
	}
	p, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return s, 0, false
	}
	return s[:i], p, true
}

// connKey is a connection: protocol and both ends, the same whichever way
// a packet goes.
type connKey struct {
	proto  string
	a, b   string
	ap, bp int
}

func connOf(k pktapKey) connKey {
	if k.src > k.dst || k.src == k.dst && k.sport > k.dport {
		return connKey{k.proto, k.dst, k.src, k.dport, k.sport}
	}
	return connKey{k.proto, k.src, k.dst, k.sport, k.dport}
}

// untagged is what forWatched couldn't attribute by tag alone.
type untagged struct {
	rescued   int64 // untagged packets claimed through their connection or port
	unclaimed int64 // untagged TCP/UDP (not DNS) to another computer that nothing claims
	flows     []string
	// Untagged and unclaimed too, but not what a leak by the app would be:
	// ICMP is sent by the kernel (neighbour discovery, errors), DNS is
	// judged from mDNSResponder's log, and the kernel's file-sharing clients
	// talk to network drives on the home network.
	icmp, dns, fileSharing int64
}

// kernelFileSharing are the ports of the network drives the kernel itself
// mounts (SMB, NetBIOS, NFS, AFP).
var kernelFileSharing = map[int]bool{445: true, 139: true, 2049: true, 548: true}

// forWatched is the packets the watched processes sent as flows: packets
// tagged with them (as proc or eproc), and untagged or kernel-tagged
// packets on a connection one of whose packets was tagged with them, or
// sent from a port one of them had open. Packets tagged with another
// process never count: on loopback, both ends of a connection are on this
// computer. mDNSResponder's packets are left out: lookups are judged from
// its log. Each flow notes whether it can be one of a VPN account's
// documented calls (only from root, while connecting).
func (r *pktapResult) forWatched(w *macWatch, local map[string]bool, mdns map[int]bool, root int, windows []window) ([]flow, untagged) {
	owner := map[connKey]int{}
	tagged := map[connKey]bool{}
	for k := range r.counts {
		if k.proc != 0 && k.proc != kernelPID || k.eproc != 0 {
			tagged[connOf(k)] = true // by a process (the kernel's tag says nothing about whose it is)
		}
		if !mdns[k.proc] && (w.has(k.proc) || w.has(k.eproc)) {
			c := connOf(k)
			if owner[c] != root {
				owner[c] = watchedPID(w, k)
			}
		}
	}
	var flows []flow
	var u untagged
	for k, s := range r.counts {
		// On loopback each packet is seen leaving and arriving, the arriving
		// copy tagged with the process receiving it: only what's sent counts.
		if k.dir == "in" || !local[k.src] || mdns[k.proc] {
			continue
		}
		c := connOf(k)
		untaggedish := (k.proc == 0 || k.proc == kernelPID) && k.eproc == 0
		pid := 0
		switch {
		case w.has(k.proc) || w.has(k.eproc):
			pid = watchedPID(w, k)
		case untaggedish && owner[c] != 0:
			pid = owner[c]
			u.rescued += s.n
		case untaggedish && (k.proto == "tcp" || k.proto == "udp") && w.portPID(k.proto, k.sport) != 0:
			pid = w.portPID(k.proto, k.sport)
			u.rescued += s.n
		case untaggedish && !tagged[c] && !local[k.dst] && !multicast(k.dst):
			switch {
			case k.proto == "icmp" || k.proto == "ipv6-icmp":
				u.icmp += s.n
			case k.dport == 53:
				u.dns += s.n
			case k.proto == "tcp" && kernelFileSharing[k.dport] && privateAddr(net.JoinHostPort(k.dst, "0")):
				// The kernel's own SMB/NFS/AFP client talking to a network
				// drive on the home network, tagged with the kernel or with
				// nothing (reconnecting, NetBIOS on 139): the app has no client
				// for these; it reaches the drive through the mount.
				u.fileSharing += s.n
			default:
				u.unclaimed += s.n
				u.flows = append(u.flows, fmt.Sprintf("%s %s > %s (%d packets)", strings.ToUpper(k.proto),
					net.JoinHostPort(k.src, strconv.Itoa(k.sport)), net.JoinHostPort(k.dst, strconv.Itoa(k.dport)), s.n))
			}
			continue
		default:
			continue // someone else's
		}
		f := flow{proto: k.proto, src: k.src, dst: k.dst, sport: k.sport, dport: k.dport, packets: s.n}
		switch {
		case pid != root:
			f.notDocumented = "not from the app's main process"
		case !within(windows, s.first, s.last):
			f.notDocumented = "not while it was connecting to the VPN"
		}
		flows = append(flows, f)
	}
	sort.Strings(u.flows)
	return flows, u
}

// appChildren are the programs the app may start on macOS (open for Finder
// and links, clamscan, ditto/plutil/codesign/sh for updating). A process
// is only added to the watched ones once a second; one of these that
// started, sent and exited in between counts as the app's.
var appChildren = map[string]bool{"open": true, "clamscan": true, "ditto": true, "plutil": true, "codesign": true, "sh": true}

// sentOffMachine reports whether pid sent packets to another computer.
func (r *pktapResult) sentOffMachine(pid int) bool {
	for k := range r.counts {
		if k.dir != "in" && (k.proc == pid || k.eproc == pid) && !net.ParseIP(k.dst).IsLoopback() {
			return true
		}
	}
	return false
}

// claimUnseenChildren handles processes pktap tagged that sent packets to
// another computer but were never seen running (they lived less than a
// second): ones named like a program the app starts are watched; the rest,
// which may be any program's, are listed.
func (r *pktapResult) claimUnseenChildren(w *macWatch, local map[string]bool) {
	unseen := map[int]int64{}
	for k, s := range r.counts {
		if k.proc > 0 && k.dir != "in" && !w.has(k.proc) && !w.everSeen(k.proc) && !local[k.dst] {
			unseen[k.proc] += s.n
		}
	}
	for pid, n := range unseen {
		name := r.procNames[pid]
		if appChildren[name] {
			w.add(pid)
			fmt.Printf("note: %s (PID %d) wasn't seen running, but the app starts programs of that name, so its %d packets count as the app's\n", name, pid, n)
		} else {
			fmt.Printf("note: %s (PID %d) sent %d packets off this computer and wasn't seen running (it may be any program's)\n", name, pid, n)
		}
	}
}

// watchedPID is which of a packet's tags is the watched process.
func watchedPID(w *macWatch, k pktapKey) int {
	if w.has(k.proc) {
		return k.proc
	}
	return k.eproc
}

func multicast(addr string) bool {
	ip := net.ParseIP(addr)
	return ip != nil && (ip.IsMulticast() || ip.Equal(net.IPv4bcast) || strings.HasSuffix(addr, ".255"))
}

// --- mDNSResponder's log ---

type mdnsLog struct {
	*lineReader
	mu      sync.Mutex
	reqs    []mdnsRequest
	notices []string // the log saying it lost messages
}

// mdnsRequest is one lookup a client asked mDNSResponder for.
type mdnsRequest struct {
	at      time.Time
	mdnsPID int
	client  int
	hash    string // <dn:…> or <mask.hash: '…'>, or the name itself if the log shows it
}

// [R6739] DNSServiceQueryRecord START -- qname: <private> <dn:72K6SQ>, qtype: A, flags: 0x15000, interface index: 0, client pid: 16734 (SharkBiteTorren), name hash: <dn:72K6SQ>
// [R1234] getaddrinfo start -- flags: 0x0, ifindex: 0, protocols: 0, hostname: <mask.hash: 'Zm9v'>, options: 0x0, client pid: 99 (nscurl)
var (
	mdnsStart  = regexp.MustCompile(`mDNSResponder\[(\d+)[^\]]*\].*?\] \S+ (?i:start) -- (.*)$`)
	mdnsClient = regexp.MustCompile(`\bclient pid: (\d+)`)
	mdnsHash   = regexp.MustCompile(`<dn:([^>]+)>|<mask\.hash: '([^']+)'>`)
	mdnsName   = regexp.MustCompile(`\b(?:qname|hostname): ([^,<\s]+)`)
	logDropped = regexp.MustCompile(`(?i)\bdropp?ed\b|\bmessages? (?:were )?lost\b`)
)

func startMDNSLog() (*mdnsLog, error) {
	l := &mdnsLog{}
	lr, err := startLines([]string{"log", "stream", "--style", "compact", "--level", "default",
		"--predicate", `process == "mDNSResponder" AND eventMessage CONTAINS[c] " start -- "`},
		func(line string) {
			r, ok := parseMDNSLine(line)
			l.mu.Lock()
			defer l.mu.Unlock()
			if ok {
				l.reqs = append(l.reqs, r)
			} else if logDropped.MatchString(line) {
				l.notices = append(l.notices, line)
			}
		},
		func(line string) bool { return strings.HasPrefix(line, "Filtering the log data") })
	if err != nil {
		return nil, err
	}
	l.lineReader = lr
	return l, nil
}

func parseMDNSLine(line string) (mdnsRequest, bool) {
	m := mdnsStart.FindStringSubmatch(line)
	if m == nil {
		return mdnsRequest{}, false
	}
	c := mdnsClient.FindStringSubmatch(m[2])
	if c == nil {
		return mdnsRequest{}, false
	}
	r := mdnsRequest{}
	if len(line) >= 23 {
		// "2026-10-21 00:59:24.799 Df mDNSResponder[656:145d] …", local time
		r.at, _ = time.ParseInLocation("2006-01-02 15:04:05.000", line[:23], time.Local)
	}
	r.mdnsPID, _ = strconv.Atoi(m[1])
	r.client, _ = strconv.Atoi(c[1])
	if h := mdnsHash.FindStringSubmatch(m[2]); h != nil {
		r.hash = h[1] + h[2]
	} else if n := mdnsName.FindStringSubmatch(m[2]); n != nil {
		r.hash = strings.ToLower(strings.TrimSuffix(n[1], "."))
	} else {
		// A request whose name this check can't read: recorded anyway, so a
		// watched client's counts (as a name the check doesn't know).
		r.hash = "unreadable request: " + strings.TrimSpace(m[2])
	}
	return r, true
}

// finish stops the log and returns the requests, and any notices of lost
// messages.
func (l *mdnsLog) finish() ([]mdnsRequest, []string) {
	l.stop()
	<-l.done
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reqs, l.notices
}

func mdnsPIDs(reqs []mdnsRequest) map[int]bool {
	m := map[int]bool{}
	for _, r := range reqs {
		m[r.mdnsPID] = true
	}
	if out, err := systool.Command("pgrep", "-x", "mDNSResponder").Output(); err == nil {
		for _, f := range strings.Fields(string(out)) {
			if p, err := strconv.Atoi(f); err == nil {
				m[p] = true
			}
		}
	}
	return m
}

// hashOf is the hash of t's name: that of the first request this process
// made while looking it up ("" if there was none, e.g. localhost answered
// without asking mDNSResponder).
func hashOf(reqs []mdnsRequest, own int, t timedLookup) string {
	const slack = 50 * time.Millisecond // the log's timestamps are to the millisecond
	for _, r := range reqs {
		if r.client == own && !r.at.Before(t.from.Add(-slack)) && !r.at.After(t.to.Add(slack)) {
			return r.hash
		}
	}
	return ""
}

// watchedLookups names the watched processes' lookups, using the hashes of
// the names this process looked up (learned), and gives each documented one
// the addresses DNS returned for it. A documented name looked up by another
// process than root, or outside the connect windows, is reported as such
// (and so not allowed). localhost isn't counted: it's answered on this
// computer.
func watchedLookups(reqs []mdnsRequest, w *macWatch, own, root int, windows []window, learned []timedLookup, answers map[string][]string) map[string][]string {
	names := map[string]string{} // hash → name
	for _, t := range learned {
		if h := hashOf(reqs, own, t); h != "" {
			names[h] = t.name
		}
	}
	out := map[string][]string{}
	for _, r := range reqs {
		if !w.has(r.client) {
			continue
		}
		name := names[r.hash]
		if name == "" {
			if strings.Contains(r.hash, ".") || r.hash == "localhost" {
				name = r.hash // the log showed the name itself
			} else {
				name = "a name the check doesn't know (hash " + r.hash + ")"
			}
		}
		if name == "localhost" {
			continue
		}
		if _, documented := documentedDirect[name]; documented {
			switch {
			case r.client != root:
				name += " (by another process than the app's main one)"
			case !within(windows, r.at, r.at):
				name += " (while not connecting to the VPN)"
			}
		}
		out[name] = dedupe(append(out[name], answers[name]...))
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- the check's own test traffic ---

// controls are a UDP packet to this computer and a system DNS lookup of a
// name nobody else asks for, sent by this process at the start and again at
// the end. tcpdump must have recorded the packets, tagged with this process,
// and mDNSResponder's log the lookups, with this process as the client: that
// proves both worked, and attributed traffic, for the whole run.
type controls struct {
	packets []string // "127.0.0.1:9 from 127.0.0.1:port"
	lookups []timedLookup
}

func (c *controls) run() {
	if conn, err := net.Dial("udp", "127.0.0.1:9"); err == nil {
		conn.Write([]byte("leakcheck"))
		c.packets = append(c.packets, conn.RemoteAddr().String()+" from "+conn.LocalAddr().String())
		conn.Close()
	}
	name := "leakcheck-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".example.com"
	c.lookups = append(c.lookups, lookupFor(name, 5*time.Second))
}

// missing returns the controls that weren't recorded.
func (c *controls) missing(r *pktapResult, reqs []mdnsRequest, own int) []string {
	var out []string
	for _, s := range c.packets {
		var dst, src string
		fmt.Sscanf(s, "%s from %s", &dst, &src)
		found := false
		for k := range r.counts {
			if k.proc == own && k.proto == "udp" && net.JoinHostPort(k.dst, strconv.Itoa(k.dport)) == dst &&
				net.JoinHostPort(k.src, strconv.Itoa(k.sport)) == src {
				found = true
			}
		}
		if !found {
			out = append(out, "the UDP packet to "+dst+" in tcpdump")
		}
	}
	for _, t := range c.lookups {
		if hashOf(reqs, own, t) == "" {
			out = append(out, "the lookup of "+t.name+" in the DNS log")
		}
	}
	return out
}
