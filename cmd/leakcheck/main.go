// Command leakcheck verifies that nothing leaves this computer outside the
// VPN tunnel. It's part of the release checklist (RELEASING.md). On Linux and
// macOS it works differently (see linux.go and darwin.go). On Windows it must run as administrator: it turns on Windows' own logging
// of network connections, process creation and DNS lookups for the duration,
// then puts the previous settings back.
//
//	go run ./cmd/leakcheck -selftest <magnet|file.torrent>   must report "selftest OK"
//	go run ./cmd/leakcheck [-seconds 60] <magnet|file.torrent>   must PASS
//	go run ./cmd/leakcheck -app [-seconds 150]                  must PASS
//
// The torrent form exercises the engine in VPN mode (WARP, on a throwaway
// account): it downloads the torrent, loads every provider's server list and
// searches the user's Prowlarr/Jackett if one is set up. -selftest also plants
// a short direct connection and a system DNS lookup, and succeeds only if
// both are caught.
//
// -app launches the installed app itself (as the logged-in user, not as
// administrator) after auditing is on, so its startup, first VPN connection
// and WebView2's first-launch traffic are all covered, then watches it and
// every process it starts while you use it. Quit the app before running it.
// With a VPN account, the server is only chosen at connect time; the app
// records it while the check runs (see startEndpointRecording).
//
// How it sees everything:
//   - Windows Filtering Platform auditing (Security events 5156/5157) records
//     every connection the OS permits or blocks, TCP and UDP, however short,
//     with the process ID.
//   - Process creation auditing (event 4688) records every process the app
//     starts, however briefly it lives.
//   - The DNS Client log (event 3006) records lookups through the system
//     resolver with the calling process.
//
// Allowed: anything to this computer (loopback), UDP between the main
// process and the VPN server's WireGuard endpoint, and a VPN account's
// documented direct calls while connecting (documentedDirect: lookups of
// those hosts from the main process, HTTPS to the addresses they returned,
// PIA's key registration with the VPN server). Everything else, from any
// of the watched processes, is reported: including UDP from WebView2 (QUIC,
// DNS sent directly by its own resolver, WebRTC, mDNS/SSDP).
package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sharkbite/internal/engine"
	"sharkbite/internal/search"
	"sharkbite/internal/secret"
	"sharkbite/internal/systool"
	"sharkbite/internal/tunnel"
	"sharkbite/internal/vpnprov"
	"sharkbite/internal/warp"
)

// documentedDirect are the hosts the app contacts over the normal
// connection by design, over HTTPS, while connecting a VPN account (no
// tunnel exists yet to go through; see vpnprov.apiDial, tunnel.DirectTrace
// and warp.Register). Kept here, not read from the app, so the check doesn't
// take the app's word for what's allowed. PIA's key registration, to the
// chosen server's own IP on port 1337 (no lookup), is allowed separately.
var documentedDirect = map[string]string{
	"www.privateinternetaccess.com": "PIA sign-in",
	"serverlist.piaservers.net":     "PIA server list",
	"api.nordvpn.com":               "NordVPN API",
	"api.mullvad.net":               "Mullvad API",
	"api.ivpn.net":                  "IVPN API",
	"www.cloudflare.com":            "real IP check (Cloudflare trace)",
	"api.cloudflareclient.com":      "WARP registration",
}

// piaKeyPort is where PIA's servers take WireGuard key registrations.
const piaKeyPort = 1337

const (
	wfpSubcategory     = "Filtering Platform Connection"
	processSubcategory = "Process Creation"
	// Process Termination (event 4689) tells when each watched process ended,
	// so a later process that Windows gives the same ID isn't taken for it.
	processEndSubcategory = "Process Termination"
	dnsLog                = "Microsoft-Windows-DNS-Client/Operational"
	appImage              = "SharkBiteTorrent.exe"

	// Planted by -selftest; the check must report both.
	plantedConn   = "1.1.1.1:443"
	plantedLookup = "example.com"
)

func main() { os.Exit(run()) }

// run does the work and returns the exit code, so deferred cleanup (putting
// the auditing settings back) always happens; os.Exit would skip it.
func run() int {
	secs := flag.Int("seconds", 0, "how long to run (default 60, or 150 with -app)")
	selftest := flag.Bool("selftest", false, "plant a direct connection and a DNS lookup; succeeds only if both are caught")
	app := flag.Bool("app", false, "launch and watch the installed app instead of running the engine")
	exe := flag.String("exe", defaultExe(), "-app: the app to launch")
	endpointFlag := flag.String("endpoint", "", "-app: the VPN server's WireGuard endpoint ip:port (default: read from the app's WARP or WireGuard settings)")
	child := flag.String("child", "", "") // Linux, macOS: the engine run as the watched process (internal)
	flag.Parse()
	if runtime.GOOS == "linux" {
		return runLinux(*secs, *selftest, *app, *exe, *endpointFlag, *child)
	}
	if runtime.GOOS == "darwin" {
		return runDarwin(*secs, *selftest, *app, *exe, *endpointFlag, *child)
	}
	if runtime.GOOS != "windows" {
		return fatalCode("leakcheck runs on Windows, Linux and macOS only")
	}
	if !isAdmin() {
		return fatalCode("run this from an administrator terminal: it needs to turn on Windows' connection auditing")
	}
	if *secs == 0 {
		*secs = 60
		if *app {
			*secs = 150
		} else if *selftest {
			*secs = 10
		}
	}

	// The endpoint UDP may go to, from the main process only; and the remote
	// control's port, if it's on, for inbound home-network connections.
	var endpoint, remotePort string
	var remoteTailscale bool // Tailscale access on: Tailscale devices may connect in too
	var acc *warp.Account
	if *app {
		if findProcess(appImage) != 0 {
			return fatalCode("quit Shark BiteTorrent first (tray icon → Quit): this check launches it itself, to cover its startup")
		}
		if _, err := os.Stat(*exe); err != nil {
			return fatalCode("app not found at " + *exe + " (use -exe)")
		}
		endpoint = *endpointFlag
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
		if flag.NArg() != 1 {
			return fatalCode("usage: leakcheck [-selftest] [-seconds N] <magnet|file.torrent>   or   leakcheck -app")
		}
		// A throwaway WARP account. Registering it is a direct API call by
		// design (it's how a WARP tunnel comes to exist), so it happens
		// before the auditing starts.
		tmp, err := os.MkdirTemp("", "leakcheck")
		if err != nil {
			return fatalCode(err.Error())
		}
		defer os.RemoveAll(tmp)
		if acc, err = warp.LoadOrRegister(filepath.Join(tmp, "warp.json")); err != nil {
			return fatalCode("registering a test WARP account: " + err.Error())
		}
		endpoint = acc.Endpoint
	}
	if endpoint != "" {
		endpoint = normalizeHostPort(endpoint)
	}

	readEndpoints := func() []string { return nil }
	if *app {
		readEndpoints = startEndpointRecording()
	}
	defer func() { stopEndpointRecording() }()

	restore, err := startAuditing()
	if err != nil {
		return fatalCode("turning on auditing: " + err.Error())
	}
	defer restore()
	// Ctrl+C mustn't leave system-wide auditing on.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Println("\ninterrupted")
		restore()
		stopEndpointRecording()
		os.Exit(130)
	}()
	time.Sleep(time.Second) // let the audit policy take effect
	start := time.Now().UTC()

	// Controls: a connection and a DNS lookup of our own, at the start and
	// again just before the logs are read, which the logs must contain. They
	// prove auditing worked for the whole run (it wasn't switched off, the
	// logs didn't drop or overwrite events), so silence from the watched
	// processes means clean rather than "not logged".
	controlStart := controlConnection()
	controlLookup()

	var root int
	var polled *pidSet
	if *app {
		root, polled, err = runApp(*exe, *secs)
	} else {
		root = os.Getpid()
		err = runEngine(acc, *secs, *selftest, flag.Arg(0))
	}
	if err != nil {
		return fatalCode(err.Error())
	}
	endControls := time.Now().UTC()
	controlEnd := controlConnection()
	controlLookup()
	time.Sleep(3 * time.Second) // let the last events reach the logs

	endpoints := endpointSet(endpoint, readEndpoints())
	if len(endpoints) == 0 {
		return fatalCode("the app didn't record a VPN connection during the run (did it connect?); pass -endpoint ip:port")
	}
	rootImage := "" // engine mode: the root is this check itself
	if *app {
		rootImage = appImage
	}
	watched, err := descendants(start, root, rootImage)
	if err != nil {
		return fatalCode("reading the process log: " + err.Error())
	}
	if polled != nil {
		watched.merge(polled)
	}
	conns, err := connectionEvents(start, watched)
	if err != nil {
		return fatalCode("reading the connection log: " + err.Error())
	}
	allLookups, dnsErr := dnsEvents(start)
	var lookups, allowed []string
	// A VPN account's documented direct calls while connecting, from the
	// main process: HTTPS to what their lookups returned, and PIA's key
	// registration with the VPN server.
	https, answersErr := documentedAnswers(start, root)
	vpnServers := map[string]bool{}
	for ep := range endpoints {
		if h, _, err := net.SplitHostPort(ep); err == nil {
			vpnServers[h] = true
		}
	}
	ownPID := os.Getpid()
	dnsControls := map[bool]bool{} // start, end
	for _, l := range allLookups {
		if l.pid == ownPID && strings.EqualFold(l.name, "localhost") {
			dnsControls[!l.at.Before(endControls)] = true
		}
		if why, ok := documentedDirect[strings.ToLower(strings.TrimSuffix(l.name, "."))]; ok && l.pid == root {
			allowed = append(allowed, "lookup of "+l.name+" ("+why+")")
		} else if watched.hasAt(l.pid, l.at, "") {
			lookups = append(lookups, l.name)
		}
	}
	controlsSeen := map[string]bool{}
	if own, err := connectionEvents(start, newPIDSet(ownPID)); err == nil {
		for _, c := range own {
			for _, ctl := range []string{controlStart, controlEnd} {
				if c.remote == ctl || c.local == ctl {
					controlsSeen[ctl] = true
				}
			}
		}
	}

	var leaks []string
	seenTunnel := false
	for _, c := range conns {
		switch {
		case c.loopback():
		case remotePort != "" && *app && c.proto == "TCP" && c.pid == root && !c.outbound &&
			portOf(c.local) == remotePort && (privateAddr(c.remote) && !tailscaleAddr(c.local) ||
			remoteTailscale && tailscaleAddr(c.remote) && tailscaleAddr(c.local)):
			// The remote control (on in Settings): phones connecting in, only
			// inbound, only on that port: from private addresses to the home
			// address, or (Tailscale access on) from Tailscale addresses to the
			// Tailscale address.
		case c.proto == "UDP" && c.pid == root && (endpoints[c.remote] || endpoints[c.local]):
			seenTunnel = true
		case c.proto == "TCP" && c.pid == root && c.outbound && portOf(c.remote) == "443" && https[hostOf(c.remote)] != "":
			allowed = append(allowed, "HTTPS to "+c.remote+" ("+https[hostOf(c.remote)]+")")
		case c.proto == "TCP" && c.pid == root && c.outbound && portOf(c.remote) == strconv.Itoa(piaKeyPort) && vpnServers[hostOf(c.remote)]:
			allowed = append(allowed, "TCP to "+c.remote+" (PIA key registration with the VPN server)")
		default:
			l := c.String() + fmt.Sprintf(" at +%ds", int(c.at.Sub(start).Seconds()))
			if k := watched.kindOf(c.pid); k != "" {
				l += " [" + k + "]"
			}
			leaks = append(leaks, l)
		}
	}
	for _, q := range lookups {
		if !strings.EqualFold(q, "localhost") {
			leaks = append(leaks, "DNS lookup outside the tunnel: "+q)
		}
	}
	leaks = dedupe(leaks)

	fmt.Printf("\nwatched %d processes; connection events: %d; system DNS lookups: %v\n", watched.len(), len(conns), dedupe(lookups))
	if !controlsSeen[controlStart] || !controlsSeen[controlEnd] {
		fmt.Println("FAIL: the connection log didn't record both of the check's own test connections (start and end), so the run wasn't fully covered")
		return 1
	}
	if dnsErr == nil {
		dnsErr = answersErr
	}
	if dnsErr != nil {
		fmt.Println("FAIL: couldn't read the DNS log, so DNS lookups weren't checked:", dnsErr)
		return 1
	}
	if !dnsControls[false] || !dnsControls[true] {
		fmt.Println("FAIL: the DNS log didn't record both of the check's own test lookups (start and end), so DNS wasn't fully covered")
		return 1
	}
	if msg := checkBrowserArgs(watched); *app && msg != "" {
		fmt.Println("FAIL: " + msg)
		return 1
	}
	for _, w := range brokerActivity(start) {
		fmt.Println("note (not necessarily caused by the app):", w)
	}
	if !seenTunnel {
		// Both the engine run and -app start their tunnel during the check,
		// so its traffic must show up.
		fmt.Println("FAIL: the VPN tunnel's own traffic (UDP to " + setString(endpoints) + ") wasn't recorded, so nothing was verified")
		return 1
	}

	if *selftest {
		missed := []string{}
		if !containsAny(leaks, plantedConn) {
			missed = append(missed, "the direct connection to "+plantedConn)
		}
		if !containsAny(leaks, plantedLookup) {
			missed = append(missed, "the DNS lookup of "+plantedLookup)
		}
		if len(missed) > 0 {
			fmt.Println("selftest FAILED: the check missed", strings.Join(missed, " and "))
			return 1
		}
		fmt.Println("selftest OK: caught both planted leaks:")
		for _, l := range leaks {
			fmt.Println("  ", l)
		}
		return 0
	}

	if len(leaks) > 0 {
		fmt.Println("FAIL: traffic outside the tunnel:")
		for _, l := range leaks {
			fmt.Println("  ", l)
		}
		return 1
	}
	if allowed = dedupe(allowed); len(allowed) > 0 {
		sort.Strings(allowed)
		fmt.Println("PASS: only the VPN tunnel, this computer, and the VPN account's documented calls while connecting:")
		for _, l := range allowed {
			fmt.Println("  ", l)
		}
		return 0
	}
	fmt.Println("PASS: only the VPN tunnel and this computer were contacted; no system DNS lookups")
	return 0
}

// runEngine brings up WARP on a throwaway account and exercises the engine,
// provider APIs and search, like the app does in VPN mode.
func runEngine(acc *warp.Account, secs int, selftest bool, source string) error {
	tun, err := tunnel.Start(tunnel.Config{
		PrivateKey: acc.PrivateKey, PeerPublicKey: acc.PeerPublicKey, Endpoint: acc.Endpoint,
		Addresses: []string{acc.AddressV4, acc.AddressV6}, DNS: []string{"1.1.1.1", "1.0.0.1"},
	})
	if err != nil {
		return fmt.Errorf("tunnel: %w", err)
	}
	defer tun.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	tr, err := tun.CheckTrace(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("trace: %w", err)
	}
	fmt.Printf("tunnel up, exit %s warp=%s\n", tr.IP, tr.Warp)
	vpnprov.SetTunnel(tun.DialContext) // as the app does once connected
	defer vpnprov.SetTunnel(nil)

	out, _ := os.MkdirTemp("", "leakcheck-dl")
	defer os.RemoveAll(out)
	eng, err := engine.New(tun, engine.Options{})
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	defer eng.Close()
	hash, err := eng.Add(engine.AddOptions{Source: source, Dir: out})
	if err != nil {
		return fmt.Errorf("add: %w", err)
	}

	for _, p := range vpnprov.All() {
		prov, _ := vpnprov.Get(p.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rs, err := prov.Regions(ctx)
		cancel()
		fmt.Printf("server list %-8s %3d locations %s\n", p.ID, len(rs), errText(err))
	}
	cfgDir, _ := os.UserConfigDir()
	if data, err := secret.ReadFile(filepath.Join(cfgDir, "SharkBiteTorrent", "search.json")); err == nil {
		var sc search.Config
		if json.Unmarshal(data, &sc) == nil && sc.Kind != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			rs, err := search.Search(ctx, sc, "debian", "")
			cancel()
			fmt.Printf("search via %s: %d results %s\n", sc.Kind, len(rs), errText(err))
		}
	}
	if selftest {
		// A short connection (closed at once) and a system DNS lookup: the
		// two things a socket-polling check would miss.
		if c, err := net.DialTimeout("tcp", plantedConn, 10*time.Second); err == nil {
			c.Close()
		}
		(&net.Resolver{}).LookupHost(context.Background(), plantedLookup)
		fmt.Println("selftest: planted a short direct connection to", plantedConn, "and a system DNS lookup of", plantedLookup)
	}

	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range eng.List() {
			if s.Hash == hash {
				fmt.Printf("\r%-12s %5.1f%%  %6.0f KB/s  peers %d   ", s.State, s.Progress*100, float64(s.DownRate)/1024, s.Peers)
			}
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Println()
	eng.Remove(hash, true)
	return nil
}

// runApp launches the app as the logged-in user (through Explorer, so it
// isn't elevated like this process) and watches it for secs seconds.
func runApp(exe string, secs int) (int, *pidSet, error) {
	systool.Command("explorer", exe).Run() // explorer's exit code is meaningless
	var pid int
	for i := 0; i < 60 && pid == 0; i++ {
		time.Sleep(500 * time.Millisecond)
		pid = findProcess(appImage)
	}
	if pid == 0 {
		return 0, nil, errors.New("the app didn't start")
	}
	set := newPIDSet(pid)
	fmt.Printf("Started Shark BiteTorrent (PID %d); watching it for %d seconds.\nYOU: leave the window alone for the first minute (that covers startup). Then: search and add a result; type or paste in the magnet box; open every Settings section (type in a text field, click through VPN providers without saving); turn the search proxy on and click Check it's working (then off again if you don't use it); switch to another VPN provider and save, then back (that covers reconnecting); click Check for updates; and paste https://example.com/ in Paste links (its \"getting downloads\" window opens: let the page load, then close it).\n", pid, secs)
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for time.Now().Before(deadline) {
		set.addChildren() // backup to the process-creation log
		time.Sleep(time.Second)
	}
	fmt.Println("Done watching; the app keeps running.")
	return pid, set, nil
}

func defaultExe() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Shark BiteTorrent", appImage)
}

// appRemotePort returns the remote control's port if it's turned on, and
// whether Tailscale access is on.
func appRemotePort() (string, bool) {
	cfgDir, _ := os.UserConfigDir()
	var set struct {
		RemoteEnabled   bool `json:"remoteEnabled"`
		RemotePort      int  `json:"remotePort"`
		RemoteTailscale bool `json:"remoteTailscale"`
	}
	if data, err := os.ReadFile(filepath.Join(cfgDir, "SharkBiteTorrent", "settings.json")); err == nil {
		json.Unmarshal(data, &set)
	}
	if !set.RemoteEnabled {
		return "", false
	}
	if set.RemotePort == 0 {
		set.RemotePort = 8732
	}
	return strconv.Itoa(set.RemotePort), set.RemoteTailscale
}

// tailscaleAddr reports whether hp is in Tailscale's range (100.64/10).
func tailscaleAddr(hp string) bool {
	host, _, _ := net.SplitHostPort(hp)
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return cgnat.Contains(net.ParseIP(host))
}

func portOf(hp string) string {
	_, p, _ := net.SplitHostPort(hp)
	return p
}

func privateAddr(hp string) bool {
	host, _, _ := net.SplitHostPort(hp)
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback())
}

// appEndpoint reads the WireGuard endpoint the app will connect to from its
// saved settings: the WARP account or the imported WireGuard config.
func appEndpoint() (string, error) {
	cfgDir, _ := os.UserConfigDir()
	dir := filepath.Join(cfgDir, "SharkBiteTorrent")
	var set struct {
		VpnProvider string `json:"vpnProvider"`
	}
	if data, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		json.Unmarshal(data, &set)
	}
	switch set.VpnProvider {
	case "", "warp":
		data, err := secret.ReadFile(filepath.Join(dir, "warp.json"))
		if err != nil {
			return "", fmt.Errorf("can't read the app's WARP account (%v); pass -endpoint", err)
		}
		var acc warp.Account
		if err := json.Unmarshal(data, &acc); err != nil || acc.Endpoint == "" {
			return "", errors.New("can't read the app's WARP endpoint; pass -endpoint")
		}
		return acc.Endpoint, nil
	case "custom":
		data, err := secret.ReadFile(filepath.Join(dir, "wireguard.conf"))
		if err != nil {
			return "", fmt.Errorf("can't read the app's WireGuard config (%v); pass -endpoint", err)
		}
		cfg, err := tunnel.ParseWGQuick(string(data))
		if err != nil {
			return "", err
		}
		return cfg.Endpoint, nil
	case "none":
		return "", errors.New("the app is set to No VPN; switch it to WARP for this check")
	default:
		return "", errAccountEndpoint
	}
}

// errAccountEndpoint: the app uses a VPN account, whose server is only
// chosen when it connects; recordedEndpoint reads it after the run.
var errAccountEndpoint = errors.New("the app uses a VPN account")

// startEndpointRecording makes the app note every VPN server it connects to
// during the run (leakcheck-active; see App.recordEndpoint), so switching
// provider during -app is covered. The returned func reads them and removes
// both files.
func startEndpointRecording() (read func() []string) {
	cfgDir, _ := os.UserConfigDir()
	dir := filepath.Join(cfgDir, "SharkBiteTorrent")
	flagFile, list := filepath.Join(dir, "leakcheck-active"), filepath.Join(dir, "vpn-endpoint")
	os.Remove(list)
	os.WriteFile(flagFile, nil, 0o600)
	// However the check ends (an error, Ctrl+C), the app stops recording.
	stopEndpointRecording = func() {
		os.Remove(flagFile)
		os.Remove(list)
	}
	return func() []string {
		b, _ := os.ReadFile(list)
		stopEndpointRecording()
		var out []string
		for _, l := range strings.Fields(string(b)) {
			out = append(out, normalizeHostPort(l))
		}
		return out
	}
}

// stopEndpointRecording removes the files startEndpointRecording made; run
// on every way out (deferred in run, and by the Ctrl+C handlers).
var stopEndpointRecording = func() {}

// endpointSet is the servers the tunnel may send to: the one known before
// the run plus those the app recorded.
func endpointSet(known string, recorded []string) map[string]bool {
	set := map[string]bool{}
	if known != "" {
		set[normalizeHostPort(known)] = true
	}
	for _, e := range recorded {
		set[e] = true
	}
	return set
}

func setString(set map[string]bool) string {
	var out []string
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// controlConnection makes one loopback TCP connection and returns the
// address it connected to.
func controlConnection() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return ""
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	if c, err := net.Dial("tcp", ln.Addr().String()); err == nil {
		c.Close()
	}
	return ln.Addr().String()
}

// --- auditing on/off ---

// dnsLogSize is the DNS Client log's size during the run. At the default
// (about 1 MB, oldest events overwritten) a busy machine can overwrite the
// app's startup lookups before they're read.
const dnsLogSize = 64 << 20

func startAuditing() (restore func(), err error) {
	type prev struct{ name, success, failure string }
	var saved []prev
	var dnsWasEnabled bool
	dnsPrevSize := ""
	var once sync.Once
	restore = func() {
		once.Do(func() {
			var failed []string
			for _, p := range saved {
				if out, err := systool.Command("auditpol", "/set", "/subcategory:"+p.name, "/success:"+p.success, "/failure:"+p.failure).CombinedOutput(); err != nil {
					failed = append(failed, fmt.Sprintf("%s (%v: %s)", p.name, err, strings.TrimSpace(string(out))))
				}
			}
			if dnsPrevSize != "" {
				// The size can only be changed while the log is off.
				systool.Command("wevtutil", "sl", dnsLog, "/e:false").Run()
				if err := systool.Command("wevtutil", "sl", dnsLog, "/ms:"+dnsPrevSize).Run(); err != nil {
					failed = append(failed, "DNS log size (was "+dnsPrevSize+" bytes)")
				}
			}
			if dnsWasEnabled {
				if err := systool.Command("wevtutil", "sl", dnsLog, "/e:true").Run(); err != nil {
					failed = append(failed, "DNS log on")
				}
			} else if err := systool.Command("wevtutil", "sl", dnsLog, "/e:false").Run(); err != nil {
				failed = append(failed, "DNS log off")
			}
			if len(failed) > 0 {
				fmt.Println("WARNING: couldn't restore these auditing settings; set them back by hand:", strings.Join(failed, "; "))
			} else {
				fmt.Println("(auditing settings restored)")
			}
		})
	}
	// Any failure part-way undoes what was already changed.
	fail := func(err error) (func(), error) {
		restore()
		return nil, err
	}
	for _, sub := range []string{wfpSubcategory, processSubcategory, processEndSubcategory} {
		out, err := systool.Command("auditpol", "/get", "/subcategory:"+sub, "/r").Output()
		if err != nil {
			return fail(fmt.Errorf("auditpol: %w", err))
		}
		// The CSV's Inclusion Setting: "No Auditing", "Success", "Failure" or
		// "Success and Failure".
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		setting := lines[len(lines)-1]
		saved = append(saved, prev{sub, onOff(strings.Contains(setting, "Success")), onOff(strings.Contains(setting, "Failure"))})
		if out, err := systool.Command("auditpol", "/set", "/subcategory:"+sub, "/success:enable", "/failure:enable").CombinedOutput(); err != nil {
			return fail(fmt.Errorf("auditpol: %v: %s", err, out))
		}
	}
	prevDNS, err := systool.Command("wevtutil", "gl", dnsLog).Output()
	if err != nil {
		return fail(fmt.Errorf("reading the DNS log settings: %w", err))
	}
	dnsWasEnabled = strings.Contains(string(prevDNS), "enabled: true")
	for _, line := range strings.Split(string(prevDNS), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "maxSize: "); ok {
			dnsPrevSize = strings.TrimSpace(v)
		}
	}
	if dnsPrevSize == "" {
		dnsWasEnabled = true // don't touch what we couldn't read
		return fail(errors.New("couldn't read the DNS log's size"))
	}
	// The size can only be changed while the log is off.
	systool.Command("wevtutil", "sl", dnsLog, "/e:false").Run()
	if err := systool.Command("wevtutil", "sl", dnsLog, "/ms:"+strconv.Itoa(dnsLogSize)).Run(); err != nil {
		return fail(fmt.Errorf("enlarging the DNS log: %w", err))
	}
	if err := systool.Command("wevtutil", "sl", dnsLog, "/e:true").Run(); err != nil {
		return fail(fmt.Errorf("turning on the DNS log: %w", err))
	}
	return restore, nil
}

func onOff(b bool) string {
	if b {
		return "enable"
	}
	return "disable"
}

// --- reading the logs ---

type conn struct {
	at            time.Time
	pid           int
	app           string
	outbound      bool
	proto         string
	local, remote string
	permitted     bool
}

// loopback is traffic that stays on this computer: both ends loopback,
// whichever side Windows logged as source or destination.
func (c conn) loopback() bool {
	isLoop := func(hp string) bool {
		host, _, _ := net.SplitHostPort(hp)
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return isLoop(c.local) && isLoop(c.remote)
}

func (c conn) String() string {
	dir, verdict := "inbound from", "allowed"
	if c.outbound {
		dir = "outbound to"
	}
	if !c.permitted {
		verdict = "blocked by firewall"
	}
	return fmt.Sprintf("%s %s %s (%s, PID %d %s)", c.proto, dir, c.remote, verdict, c.pid, filepath.Base(c.app))
}

type event struct {
	System struct {
		EventID     int `xml:"EventID"`
		TimeCreated struct {
			SystemTime string `xml:"SystemTime,attr"`
		} `xml:"TimeCreated"`
		Execution struct {
			ProcessID int `xml:"ProcessID,attr"`
		} `xml:"Execution"`
	} `xml:"System"`
	Data []struct {
		Name  string `xml:"Name,attr"`
		Value string `xml:",chardata"`
	} `xml:"EventData>Data"`
}

// field looks a value up by name, ignoring case (Windows logs "ProcessId"
// for events 5156/5157; Microsoft's documentation writes "ProcessID").
func (e event) field(name string) string {
	for _, d := range e.Data {
		if strings.EqualFold(d.Name, name) {
			return d.Value
		}
	}
	return ""
}

func queryLog(logName, xpath string) ([]event, error) {
	out, err := systool.Command("wevtutil", "qe", logName, "/q:"+xpath, "/f:xml").Output()
	if err != nil {
		return nil, err
	}
	var doc struct {
		Events []event `xml:"Event"`
	}
	if err := xml.Unmarshal([]byte("<Events>"+string(out)+"</Events>"), &doc); err != nil {
		return nil, err
	}
	return doc.Events, nil
}

func since(t time.Time) string {
	return "TimeCreated[@SystemTime>='" + t.Format("2006-01-02T15:04:05.000Z") + "']"
}

// descendants returns root and every process started (directly or not) by
// it since start, each with when it ran, from the process-creation and
// -termination logs (events 4688 and 4689). Windows reuses process IDs
// quickly: a WebView2 process that exits can hand its ID to some other
// program a moment later (seen: NVIDIA's nvcontainer.exe), whose traffic
// must not count as the app's.
func descendants(start time.Time, root int, rootImage string) (*pidSet, error) {
	created, err := queryLog("Security", "*[System[EventID=4688 and "+since(start)+"]]")
	if err != nil {
		return nil, err
	}
	ended, err := queryLog("Security", "*[System[EventID=4689 and "+since(start)+"]]")
	if err != nil {
		return nil, err
	}
	var evs []procEvent
	at := func(e event) time.Time {
		t, _ := time.Parse(time.RFC3339Nano, e.System.TimeCreated.SystemTime)
		return t
	}
	for _, e := range created {
		child, err1 := parseHexPID(e.field("NewProcessId"))
		parent, err2 := parseHexPID(e.field("ProcessId"))
		if err1 == nil && err2 == nil {
			evs = append(evs, procEvent{at: at(e), pid: child, parent: parent, image: imageName(e.field("NewProcessName"))})
		}
	}
	for _, e := range ended {
		if pid, err := parseHexPID(e.field("ProcessId")); err == nil {
			evs = append(evs, procEvent{at: at(e), pid: pid, exit: true})
		}
	}
	return processTree(root, rootImage, evs), nil
}

// procEvent is a process starting (with its parent) or exiting.
type procEvent struct {
	at          time.Time
	pid, parent int
	image       string
	exit        bool
}

// processTree follows root's descendants through starts and exits in time
// order: a process counts only if it started while its parent was one of
// them, and only for as long as it ran. root counts from the start of the
// run (its own start event, by a parent that isn't watched, is no reason to
// drop it); rootImage is its program's file name, "" for any (engine mode,
// where the root is the check itself).
func processTree(root int, rootImage string, evs []procEvent) *pidSet {
	slices.SortStableFunc(evs, func(a, b procEvent) int { return a.at.Compare(b.at) })
	set := &pidSet{pids: map[int]bool{root: true}, spans: map[int][]span{root: {{image: strings.ToLower(rootImage)}}}}
	alive := map[int]bool{root: true}
	rootEnded := false
	for _, e := range evs {
		switch {
		case e.pid == root && !rootEnded:
			if e.exit { // the root itself quit: its ID could be reused after
				set.spans[root][0].to = e.at
				alive[root], rootEnded = false, true
			}
		case !e.exit && alive[e.parent]:
			set.pids[e.pid] = true
			set.spans[e.pid] = append(set.spans[e.pid], span{from: e.at, image: e.image})
			alive[e.pid] = true
		case !e.exit:
			alive[e.pid] = false // an ID reused by something else
		case alive[e.pid]:
			sp := set.spans[e.pid]
			sp[len(sp)-1].to = e.at
			alive[e.pid] = false
		}
	}
	return set
}

// imageName is a program's file name, lower case ("msedgewebview2.exe"),
// from a full path in any form Windows logs it.
func imageName(path string) string {
	if i := strings.LastIndexAny(path, `\/`); i >= 0 {
		path = path[i+1:]
	}
	return strings.ToLower(path)
}

func parseHexPID(s string) (int, error) {
	// Windows process IDs fit in a signed 32-bit int, so this can't overflow.
	n, err := strconv.ParseInt(strings.TrimPrefix(strings.ToLower(s), "0x"), 16, 32)
	return int(n), err
}

func connectionEvents(start time.Time, pids *pidSet) ([]conn, error) {
	evs, err := queryLog("Security", "*[System[(EventID=5156 or EventID=5157) and "+since(start)+"]]")
	if err != nil {
		return nil, err
	}
	var out []conn
	for _, e := range evs {
		pid, _ := strconv.Atoi(e.field("ProcessId"))
		at, _ := time.Parse(time.RFC3339Nano, e.System.TimeCreated.SystemTime)
		if !pids.hasAt(pid, at, imageName(e.field("Application"))) {
			continue
		}
		proto := map[string]string{"6": "TCP", "17": "UDP"}[e.field("Protocol")]
		if proto == "" {
			proto = "protocol " + e.field("Protocol")
		}
		outbound := e.field("Direction") == "%%14593"
		src := normalizeHostPort(net.JoinHostPort(e.field("SourceAddress"), e.field("SourcePort")))
		dst := normalizeHostPort(net.JoinHostPort(e.field("DestAddress"), e.field("DestPort")))
		// For inbound traffic Windows lists this computer as the destination.
		local, remote := src, dst
		if !outbound {
			local, remote = dst, src
		}
		out = append(out, conn{
			at: at, pid: pid, app: e.field("Application"), outbound: outbound, proto: proto,
			local: local, remote: remote, permitted: e.System.EventID == 5156,
		})
	}
	return out, nil
}

type lookup struct {
	at   time.Time
	pid  int
	name string
}

// documentedAnswers returns the addresses the system resolver answered to
// pid's lookups of documentedDirect hosts since start (DNS Client event
// 3008), with what each call is for.
func documentedAnswers(start time.Time, pid int) (map[string]string, error) {
	evs, err := queryLog(dnsLog, "*[System[EventID=3008 and "+since(start)+"]]")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range evs {
		why, ok := documentedDirect[strings.ToLower(strings.TrimSuffix(e.field("QueryName"), "."))]
		if !ok || e.System.Execution.ProcessID != pid {
			continue
		}
		for _, a := range parseQueryResults(e.field("QueryResults")) {
			out[a] = why
		}
	}
	return out, nil
}

// parseQueryResults reads the addresses in event 3008's QueryResults:
// entries separated by ";", IPv4 shown as "::ffff:1.2.3.4", aliases as
// "type:  5 name".
func parseQueryResults(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ";") {
		if ip := net.ParseIP(strings.TrimSpace(f)); ip != nil {
			out = append(out, ip.String()) // ::ffff:a.b.c.d prints as a.b.c.d
		}
	}
	return out
}

func hostOf(hp string) string {
	h, _, _ := net.SplitHostPort(hp)
	return h
}

// dnsEvents returns every system-resolver lookup since start, by any process.
func dnsEvents(start time.Time) ([]lookup, error) {
	evs, err := queryLog(dnsLog, "*[System[EventID=3006 and "+since(start)+"]]")
	if err != nil {
		return nil, err
	}
	var out []lookup
	for _, e := range evs {
		at, _ := time.Parse(time.RFC3339Nano, e.System.TimeCreated.SystemTime)
		out = append(out, lookup{at: at, pid: e.System.Execution.ProcessID, name: e.field("QueryName")})
	}
	return out, nil
}

// controlLookup is a system-resolver lookup of our own that the DNS log
// must record (see the controls in run).
func controlLookup() {
	(&net.Resolver{}).LookupHost(context.Background(), "localhost")
}

// checkBrowserArgs confirms WebView2's browser process got the app's
// arguments. Chromium uses only the last --disable-features, so if anything
// adds one after ours, all of ours are silently dropped.
func checkBrowserArgs(s *pidSet) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for pid, cmd := range s.cmd {
		if !strings.Contains(strings.ToLower(s.kind[pid]), "msedgewebview2.exe") || strings.Contains(cmd, "--type=") {
			continue
		}
		// The file-host window ("Paste links") reaches the network, but only
		// through the app's own proxy into the VPN, resolving nothing itself.
		hostWindow := strings.Contains(cmd, "--proxy-server=socks5://127.0.0.1:")
		if !hostWindow {
			found = true
		}
		last := ""
		for _, f := range strings.Fields(cmd) {
			if v, ok := strings.CutPrefix(f, "--disable-features="); ok {
				last = v
			}
		}
		for _, need := range []string{"msImplicitSignin", "msEdgeOSAccountInfoSubstrate", "TextPrediction", "msSmartScreenProtection", "WebRtcHideLocalIpsWithMdns"} {
			if !strings.Contains(","+last+",", ","+need+",") {
				return "WebView2 (PID " + strconv.Itoa(pid) + ") didn't get the app's --disable-features (missing " + need + "); its background features are on"
			}
		}
		needs := []string{"--proxy-server=127.0.0.1:9", "--host-resolver-rules="}
		if hostWindow {
			needs = []string{"--host-resolver-rules=", "MAP * ~NOTFOUND", "--disable-quic", "--force-webrtc-ip-handling-policy=disable_non_proxied_udp", "--proxy-bypass-list=<-loopback>"}
			if strings.Count(cmd, "--proxy-server=") != 1 {
				return "the file-host window's WebView2 (PID " + strconv.Itoa(pid) + ") has more than one --proxy-server (the last one wins)"
			}
		}
		for _, need := range needs {
			if !strings.Contains(cmd, need) {
				return "WebView2 (PID " + strconv.Itoa(pid) + ") didn't get " + need
			}
		}
	}
	if !found {
		return "didn't see WebView2's browser process, so its settings weren't checked"
	}
	return ""
}

// brokerActivity lists connections made during the run by Windows' account
// broker and proxy auto-discovery services. They run outside the app's
// process tree, so the app's WebView2 could use them without the check
// seeing it; this only reports them (the rest of Windows uses them too).
func brokerActivity(start time.Time) []string {
	set := &pidSet{pids: map[int]bool{}}
	for _, svc := range []string{"TokenBroker", "WinHttpAutoProxySvc"} {
		out, _ := systool.Command("tasklist", "/SVC", "/FI", "SERVICES eq "+svc, "/FO", "CSV", "/NH").Output()
		if f := strings.Split(strings.TrimSpace(string(out)), "\",\""); len(f) > 1 {
			if pid, err := strconv.Atoi(f[1]); err == nil {
				set.pids[pid] = true
			}
		}
	}
	if pid := findProcess("Microsoft.AAD.BrokerPlugin.exe"); pid != 0 {
		set.pids[pid] = true
	}
	conns, err := connectionEvents(start, set)
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range conns {
		if !c.loopback() {
			out = append(out, c.String())
		}
	}
	return dedupe(out)
}

// normalizeHostPort makes "ip:port" comparable however Windows or the app
// wrote the address (IPv6 zero compression, brackets).
func normalizeHostPort(hp string) string {
	host, port, err := net.SplitHostPort(hp)
	if err != nil {
		return hp
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	return net.JoinHostPort(host, port)
}

// --- processes ---

type pidSet struct {
	mu   sync.Mutex
	pids map[int]bool
	kind map[int]string // e.g. "msedgewebview2.exe --type=utility --utility-sub-type=…"
	cmd  map[int]string // full command line
	// spans: when each process with that ID was one of the watched ones
	// (none known: any time, as for the check's own process).
	spans map[int][]span
	// lastPoll: when addChildren last looked (a process it finds now may
	// have started any time since).
	lastPoll time.Time
}

// span is one process's life under an ID; to is zero while it runs.
type span struct {
	from, to time.Time
	image    string // lower-case file name; "" if unknown
	ended    bool   // addChildren saw another program under the ID
}

// spanSlack covers the logs' and the polling's timing being a little off.
const spanSlack = time.Second

// hasAt reports whether the event at t from pid (image: its program's file
// name, if the event names it) came from a watched process.
func (s *pidSet) hasAt(pid int, t time.Time, image string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp := s.spans[pid]
	if len(sp) == 0 {
		return s.pids[pid]
	}
	for _, x := range sp {
		if t.Before(x.from.Add(-spanSlack)) || !x.to.IsZero() && t.After(x.to.Add(spanSlack)) {
			continue
		}
		if image == "" || x.image == "" || strings.EqualFold(image, x.image) {
			return true
		}
	}
	return false
}

// newPIDSet watches pid, for its whole life (it's the check itself or the
// app it launched, both running from the start of the run), and the
// processes it starts.
func newPIDSet(pid int) *pidSet {
	s := &pidSet{pids: map[int]bool{pid: true}, spans: map[int][]span{pid: {{}}}}
	s.addChildren()
	return s
}

func (s *pidSet) has(pid int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pids[pid]
}

func (s *pidSet) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pids)
}

func (s *pidSet) merge(o *pidSet) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kind == nil {
		s.kind, s.cmd = map[int]string{}, map[int]string{}
	}
	if s.spans == nil {
		s.spans = map[int][]span{}
	}
	for p := range o.pids {
		s.pids[p] = true
		if k := o.kind[p]; k != "" {
			s.kind[p], s.cmd[p] = k, o.cmd[p]
		}
		// The logs know best when a process ran; the polling's spans fill
		// in for any the logs missed.
		if len(s.spans[p]) == 0 {
			s.spans[p] = o.spans[p]
		}
	}
}

// addChildren adds every running descendant of the watched processes.
func (s *pidSet) addChildren() {
	out, err := systool.Command("powershell", "-NoProfile", "-Command",
		"Get-CimInstance Win32_Process | ForEach-Object { \"$($_.ProcessId) $($_.ParentProcessId) $($_.Name) $($_.CommandLine)\" }").Output()
	if err != nil {
		return
	}
	now := time.Now().UTC()
	parent := map[int][]int{}
	kinds := map[int]string{}
	cmds := map[int]string{}
	images := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			p, _ := strconv.Atoi(f[0])
			pp, _ := strconv.Atoi(f[1])
			parent[pp] = append(parent[pp], p)
			if len(f) >= 3 {
				kinds[p] = processKind(f[2], strings.Join(f[3:], " "))
				cmds[p] = strings.Join(f[3:], " ")
				images[p] = strings.ToLower(f[2])
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kind == nil {
		s.kind, s.cmd = map[int]string{}, map[int]string{}
	}
	if s.spans == nil {
		s.spans = map[int][]span{}
	}
	since := s.lastPoll
	if since.IsZero() {
		since = now.Add(-5 * time.Second)
	}
	s.lastPoll = now
	for p := range s.pids {
		if k, ok := kinds[p]; ok && !s.reusedLocked(p, images[p]) {
			s.kind[p], s.cmd[p] = k, cmds[p]
		}
		s.seenLocked(p, images[p], now, since)
	}
	for changed := true; changed; {
		changed = false
		for p := range s.pids {
			if s.reusedLocked(p, images[p]) {
				continue // not ours any more: neither are its children
			}
			for _, c := range parent[p] {
				if !s.pids[c] {
					s.pids[c], changed = true, true
					s.kind[c], s.cmd[c] = kinds[c], cmds[c]
					s.seenLocked(c, images[c], now, since)
				}
			}
		}
	}
}

// seenLocked extends pid's polled span to now (a new one starting at
// since, the previous poll: it may have started any time after), or ends it if another
// program now runs under that ID (or nothing does).
func (s *pidSet) seenLocked(pid int, image string, now, since time.Time) {
	sp := s.spans[pid]
	if len(sp) == 0 {
		if image != "" {
			s.spans[pid] = []span{{from: since, to: now, image: image}}
		}
		return
	}
	last := &sp[len(sp)-1]
	switch {
	case last.from.IsZero() && last.to.IsZero():
		// a root (newPIDSet, processTree): watched for its whole life
	case last.ended:
	case image == "" || last.image != "" && image != last.image:
		last.ended = true // exited, or the ID was reused
	default:
		last.to = now
	}
}

// reusedLocked: the program now running under pid isn't the watched one.
func (s *pidSet) reusedLocked(pid int, image string) bool {
	sp := s.spans[pid]
	if len(sp) == 0 || image == "" {
		return false
	}
	last := sp[len(sp)-1]
	if last.from.IsZero() && last.to.IsZero() {
		return false // a root
	}
	return last.ended || last.image != "" && last.image != image
}

// processKind is the image name plus Chromium's process-type switches.
func processKind(name, cmdline string) string {
	k := name
	for _, f := range strings.Fields(cmdline) {
		if strings.HasPrefix(f, "--type=") || strings.HasPrefix(f, "--utility-sub-type=") || strings.HasPrefix(f, "--service-sandbox-type=") {
			k += " " + f
		}
	}
	return k
}

func (s *pidSet) kindOf(pid int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kind[pid]
}

func findProcess(image string) int {
	out, _ := systool.Command("tasklist", "/FI", "IMAGENAME eq "+image, "/FO", "CSV", "/NH").Output()
	f := strings.Split(strings.TrimSpace(string(out)), "\",\"")
	if len(f) > 1 {
		pid, _ := strconv.Atoi(f[1])
		return pid
	}
	return 0
}

func isAdmin() bool {
	// "net session" only succeeds in an elevated process.
	return systool.Command("net", "session").Run() == nil
}

func containsAny(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func errText(err error) string {
	if err != nil {
		return "(" + err.Error() + ")"
	}
	return ""
}

func fatalCode(msg string) int {
	fmt.Fprintln(os.Stderr, "leakcheck:", msg)
	return 2
}
