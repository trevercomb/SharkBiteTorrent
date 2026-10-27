package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"sharkbite/internal/engine"
	"sharkbite/internal/platform"
	"sharkbite/internal/scan"
	"sharkbite/internal/search"
	"sharkbite/internal/secret"
	"sharkbite/internal/socks"
	"sharkbite/internal/tunnel"
	"sharkbite/internal/vpnprov"
	"sharkbite/internal/warp"
)

type App struct {
	ctx       context.Context
	configDir string
	exe       string
	// instanceLock is held by the running copy for its whole life
	// (lockInstance). secondary: another copy holds it. On Linux, Wails runs
	// startup before handing a second launch (a magnet link) to the running
	// copy; that copy mustn't load, mark or save the torrent list, nor
	// remove the running copy's marker when it exits.
	instanceLock *os.File
	secondary    bool
	markerMu     sync.Mutex // the update-download marker (updater.go)
	accountsMu   sync.Mutex // reading, changing and saving accounts.json (accounts.go)
	// The VPN provider turned requests away (HTTP 429): none go to it
	// before limitUntil, and the app connects again by itself then
	// (accounts.go). Guarded by mu.
	limitProvider string
	limitUntil    time.Time
	limitStrikes  int
	limitTimer    *time.Timer
	stopOnce      sync.Once // stopAll
	// installing: an update is being installed (InstallUpdate); a second
	// one at the same time (a double click) is refused.
	installing atomic.Bool

	mu          sync.Mutex
	tun         *tunnel.Tunnel
	eng         *engine.Engine
	stopMonitor context.CancelFunc
	vpn         VpnStatus
	saved       []savedTorrent
	settings    Settings
	pending     []pendingSource // magnets/.torrent paths received before the VPN was up
	quitting    bool
	scanning    map[string]bool // torrents being scanned right now
	// autoVerified: torrents verified automatically this session, so "all
	// downloads" doesn't re-read one again after its repair.
	autoVerified     map[string]bool
	verifyFails      map[string]int          // auto-verify attempts that failed, by hash (see autoVerify); guarded by mu
	downloadedUpdate string                  // the update file DownloadUpdate saved (see ShowDownloadedUpdate); guarded by mu
	work             map[string]*torrentWork // background work reading a torrent's files (see workCtx)
	removing         map[string]bool         // torrents being removed; kept across engine restarts
	proxy            *socks.Server           // local SOCKS5 proxy into the tunnel, for Prowlarr/Jackett
	proxyErr         string
	// connMu serializes connecting and reconnecting, so the startup connect
	// and quick successive Reconnects can't interleave. reconnectGen lets a
	// queued Reconnect see that a newer one supersedes it.
	remote       *remoteServer // phone remote control (remote.go)
	remoteErr    string
	remotePaused string     // why it isn't running although enabled
	lanPairUntil time.Time  // a home-Wi-Fi pairing is open until then (no home network yet)
	remoteOpMu   sync.Mutex // serializes starting/stopping it
	remoteMu     sync.Mutex // guards store/storeErr
	store        remoteStore
	storeErr     error
	shuttingDown bool
	connMu       sync.Mutex
	reconnectGen atomic.Uint64
	sfvChecking  map[string]bool              // torrents whose .sfv is being checked
	sfvResults   map[string]*engine.SFVStatus // .sfv results not yet in a saved scan record
	verifyCtx    context.Context              // cancelled on disconnect; stops "Verify files"
}

type VpnStatus struct {
	State    string `json:"state"` // connecting, connected, error, off ("No VPN" chosen)
	Provider string `json:"provider"`
	Name     string `json:"name"`   // display name: "WARP", "Mullvad", "WireGuard"
	Server   string `json:"server"` // server picked by an account provider
	ExitIP   string `json:"exitIP"`
	Location string `json:"location"`
	Colo     string `json:"colo"`
	Port     int    `json:"port"`    // forwarded port accepting inbound peers, 0 if none
	Warning  string `json:"warning"` // non-fatal problem, e.g. port forwarding failed
	Error    string `json:"error"`
}

type Settings struct {
	DownloadDir   string `json:"downloadDir"`
	VpnProvider   string `json:"vpnProvider"`   // "warp", "account", "custom" or "none" (no VPN)
	ForwardedPort int    `json:"forwardedPort"` // custom only; accounts forward automatically
	// AccountProvider/AccountRegion apply when VpnProvider is "account".
	// An empty region means a server in the user's own country.
	AccountProvider string `json:"accountProvider"`
	AccountRegion   string `json:"accountRegion"`
	DownLimitKB     int64  `json:"downLimitKB"` // 0 = unlimited
	UpLimitKB       int64  `json:"upLimitKB"`
	CloseToTray     bool   `json:"closeToTray"`
	// Update checks (see updater.go).
	AutoUpdateCheck bool   `json:"autoUpdateCheck"`
	SkippedVersion  string `json:"skippedVersion"`
	LastUpdateCheck int64  `json:"lastUpdateCheck"` // unix seconds
	// UpdateChannel: "stable" (default) or "beta", which also offers
	// pre-releases (v0.3.0-beta.1). Not used by the Store version.
	UpdateChannel string `json:"updateChannel"`
	// ScanDownloads scans finished torrents with the system antivirus.
	ScanDownloads bool `json:"scanDownloads"`
	// AutoVerify: re-check a finished torrent's files against its piece
	// hashes before the malware scan. "suspect" (default): only if something
	// during the download could have damaged them; "all": every download;
	// "off": never.
	AutoVerify string `json:"autoVerify"`
	// Remote control from phones on the home network (remote.go); changed
	// through SetRemote, not SaveSettings.
	RemoteEnabled   bool `json:"remoteEnabled"`
	RemotePort      int  `json:"remotePort"`
	RemoteTailscale bool `json:"remoteTailscale"` // also listen on the Tailscale address
}

// savedTorrent is persisted so torrents survive restarts. Source is a magnet
// link or a path to a .torrent copy inside the config dir.
type savedTorrent struct {
	Hash    string `json:"hash"`
	Source  string `json:"source"`
	Dir     string `json:"dir"`
	Skipped []int  `json:"skipped,omitempty"`
	Paused  bool   `json:"paused"`
	// Confirm: added from outside the app (a browser, the file association),
	// so it waits until the user starts it. It isn't given to the engine
	// until then: nothing (trackers, DHT, peers) is contacted. Name and Size
	// are what the link or file says, for showing it meanwhile.
	Confirm  bool           `json:"confirm,omitempty"`
	Name     string         `json:"name,omitempty"`
	Size     int64          `json:"size,omitempty"`
	Warnings []scan.Warning `json:"warnings,omitempty"` // from a .torrent's file list, shown before Start
	// WarningsAccepted: the user resumed (or started) it despite warnings
	// about its files, so it isn't paused for them again (see
	// engine.AddOptions.AcceptWarnings).
	WarningsAccepted bool `json:"warningsAccepted,omitempty"`
	// Checked: saved by a version that checks a torrent's paths before it
	// writes (see engine/ownership.go); every entry this version writes has
	// it. legacy is set when loading an entry without the field, which only
	// v0.2.36 or older wrote: its existing files are adopted once.
	Checked bool `json:"checked,omitempty"`
	legacy  bool
	// Suspect: why its files should be verified before the malware scan
	// (its drive went away, a write failed, the app didn't shut down
	// normally while it downloaded); cleared once verified.
	Suspect string `json:"suspect,omitempty"`
	// Scan is the antivirus result once the torrent finished (nil = not yet).
	Scan *scanRecord `json:"scan,omitempty"`
}

func NewApp() *App {
	dir, _ := os.UserConfigDir()
	exe, _ := os.Executable()
	configDir := filepath.Join(dir, "SharkBiteTorrent")
	// Carry over data from before the app was renamed.
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		os.Rename(filepath.Join(dir, "torrent-client"), configDir)
	}
	return &App{configDir: configDir, exe: exe}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if !a.lockInstance() {
		a.secondary = true
		// Wails hands the launch to the running copy and exits; if it can't
		// (no D-Bus), this copy goes too rather than run beside it.
		go func() {
			time.Sleep(15 * time.Second)
			runtime.Quit(ctx)
		}()
		return
	}
	a.loadSettings()
	a.loadSaved()
	a.noteUncleanExit()
	a.handleArgs(os.Args[1:], true)
	if !storeBuild {
		platform.RefreshStartup(a.exe) // the app moved: keep sign-in start working
		platform.RefreshAssociations(a.exe)
	}
	search.SetTunnel(a.searchDial) // a search server that isn't local goes through the VPN
	go a.cleanUpdateTemp()
	runtime.OnFileDrop(ctx, func(_, _ int, paths []string) { a.handleArgs(paths, false) })
	go a.quitOnSignals()
	go a.runTray()
	go a.connect()
	a.startProxy()
	a.loadRemoteStore()
	a.remoteOpMu.Lock()
	a.startRemote()
	a.remoteOpMu.Unlock()
	go a.watchRemoteNetwork()
}

func (a *App) isShuttingDown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.quitting || a.shuttingDown
}

// shutdownLimit bounds quitting. Closing the engine writes out pieces held
// in memory, and on a stalled network drive closing a file can hang with no
// limit of its own. Pieces not written by then are checked and downloaded
// again at the next start, as after a crash.
const shutdownLimit = 40 * time.Second

func (a *App) shutdown(ctx context.Context) {
	a.stopAll(ctx)
	quitTray()
}

// stopAll stops the proxy, the remote control and the engine (which saves
// what's downloaded), once, within shutdownLimit. On macOS beforeClose runs
// it while the window's run loop still works (see there); elsewhere, and if
// it hasn't run, OnShutdown does.
func (a *App) stopAll(ctx context.Context) {
	a.stopOnce.Do(func() { a.stopAllNow(ctx) })
}

func (a *App) stopAllNow(ctx context.Context) {
	if a.secondary {
		return // nothing of its own was started (see startup)
	}
	a.mu.Lock()
	a.shuttingDown = true // a Reconnect in progress must not start anything new
	a.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.stopProxy()
		a.remoteOpMu.Lock()
		a.stopRemote()
		a.remoteOpMu.Unlock()
		a.disconnect()
	}()
	select {
	case <-done:
		os.Remove(a.runningMarker()) // shut down normally (see noteUncleanExit)
	case <-time.After(shutdownLimit):
		runtime.LogWarning(ctx, "quitting: saving downloads took too long; the rest is checked at the next start")
	}
}

func (a *App) runningMarker() string { return filepath.Join(a.configDir, "running") }

// noteUncleanExit: a marker left from the last run means the app didn't shut
// down normally (a crash, power loss, a forced kill), so what it was
// downloading may not all have reached the drive; those torrents are
// verified before their malware scan. Then the marker is set for this run.
func (a *App) noteUncleanExit() {
	if _, err := os.Stat(a.runningMarker()); err == nil {
		a.mu.Lock()
		for i := range a.saved {
			if s := &a.saved[i]; s.Scan == nil && s.Suspect == "" && !s.Confirm && !s.Paused {
				s.Suspect = "the app didn't shut down normally while it was downloading"
			}
		}
		a.writeSaved()
		a.mu.Unlock()
	}
	os.MkdirAll(a.configDir, 0o700)
	os.WriteFile(a.runningMarker(), nil, 0o600)
}

// beforeClose hides to the tray instead of quitting, if enabled.
// It returns true when the caller must not quit (hidden to the tray, or, on
// macOS, the run loop is stopped here — see quit_darwin.go).
func (a *App) beforeClose(ctx context.Context) bool {
	a.mu.Lock()
	hide := !hideOnClose && a.settings.CloseToTray && !a.quitting
	if !hide {
		a.quitting = true
	}
	a.mu.Unlock()
	// To the tray; or quitting, where saving what's downloaded can take a
	// while (a busy network drive), so the window goes now rather than
	// seeming frozen.
	runtime.WindowHide(ctx)
	if hide {
		return true
	}
	if hideOnClose {
		// macOS: this runs on a goroutine while the run loop still works,
		// so saving downloads (a busy network drive: up to shutdownLimit)
		// happens here, with the app still answering macOS. After the run
		// loop stops, OnShutdown runs on the main thread, and anything slow
		// there shows the beachball.
		a.stopAll(ctx)
	}
	return stopMacRunLoop()
}

func (a *App) quit() {
	a.mu.Lock()
	a.quitting = true
	a.mu.Unlock()
	runtime.Quit(a.ctx)
}

// quitOnSignals quits the usual way (shutdown closes the engine, which saves
// what's downloaded) on SIGTERM, Ctrl+C or SIGHUP: logging out, a service
// manager, `kill`. Wails catches SIGTERM and Ctrl+C itself, but quits
// through OnBeforeClose, which with Close to tray on only hid the window;
// a.quit marks the app as quitting first. A second signal exits at once, in
// case shutdown hangs.
func (a *App) quitOnSignals() {
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt, syscall.SIGHUP)
	<-sig
	go a.quit()
	<-sig
	os.Exit(1)
}

func (a *App) showWindow() {
	runtime.WindowShow(a.ctx)
	runtime.WindowUnminimise(a.ctx)
	runtime.WindowSetAlwaysOnTop(a.ctx, true) // bring to front on Windows
	runtime.WindowSetAlwaysOnTop(a.ctx, false)
}

// onSecondInstance receives the args of a second launch, e.g. when a magnet
// link is clicked in the browser while the app is already running.
func (a *App) onSecondInstance(data options.SecondInstanceData) {
	if a.isShuttingDown() {
		// Quitting (the window is already gone, saving downloads): a magnet
		// link or a relaunch arriving now would be lost with this copy.
		runtime.LogWarning(a.ctx, "ignored a launch that arrived while quitting; open it again once the app has quit")
		return
	}
	for _, arg := range data.Args {
		if arg == platform.AutostartArg {
			// A sign-in start while the app already runs (macOS: the agent
			// started again after RefreshStartup re-registered it): nothing
			// to do, and no window.
			return
		}
	}
	a.handleArgs(data.Args, true)
	if a.ctx != nil {
		a.showWindow()
	}
}

type pendingSource struct {
	src      string
	external bool
}

// handleArgs adds magnet links and .torrent files passed on the command line,
// via the file association, or dropped on the window. external ones (not
// dropped by the user) are added paused, waiting for the user to start them:
// any web page can open a magnet link.
func (a *App) handleArgs(args []string, external bool) {
	if a.isShuttingDown() {
		return // see onSecondInstance
	}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "magnet:") && !strings.EqualFold(filepath.Ext(arg), ".torrent") {
			continue
		}
		a.mu.Lock()
		ready := a.eng != nil
		if !ready {
			a.pending = append(a.pending, pendingSource{arg, external})
		}
		a.mu.Unlock()
		if ready {
			if err := a.addSourceFrom(arg, external); err != nil {
				a.notifyError(err)
			}
		}
	}
}

func (a *App) notifyError(err error) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "error", err.Error())
	}
}

func (a *App) setVpn(s VpnStatus) {
	a.mu.Lock()
	a.vpn = s
	a.mu.Unlock()
}

// connect starts the configured VPN, verifies it, then starts the torrent
// engine. Nothing is downloaded until this succeeds.
func (a *App) connect() {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	a.connectPrepared(nil)
}

// preparedConn is what connecting needs from outside the tunnel being
// built: the real IP (to confirm the new tunnel's exit differs) and, for
// VPN accounts, the provider's connection details.
type preparedConn struct {
	set          Settings // what it was prepared for
	account      string   // and with which account (see accountKey)
	realIP, home string
	conn         *vpnprov.Connection
	// customEndpoint: a WireGuard config's server name, resolved through the
	// VPN that's still up when switching ("" if not needed or no VPN was up).
	customEndpoint string
}

// prepareConn does the steps of connecting that talk to the provider's API
// (sign-in token, server choice, key registration). Provider API calls go
// through whatever tunnel is up at the time (vpnprov.SetTunnel), so
// Reconnect runs this before closing the old tunnel.
func (a *App) prepareConn(set Settings) (*preparedConn, error) {
	p := &preparedConn{set: set, account: accountKey(a.account(set.AccountProvider))}
	// Only WARP reports warp=on, so for other providers we confirm the tunnel
	// by checking that the exit IP differs from the real one. The real
	// connection's country also picks the automatic server location.
	if set.VpnProvider != "warp" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if tr, err := tunnel.DirectTrace(ctx); err == nil {
			p.realIP, p.home = tr.IP, tr.Location
		}
		cancel()
	}
	if set.VpnProvider == "account" {
		prov, ok := vpnprov.Get(set.AccountProvider)
		acc := a.account(set.AccountProvider)
		if !ok || acc == nil {
			return nil, errors.New("not signed in to a VPN account; open Settings to sign in")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := a.rateLimitedNow(set.AccountProvider); err != nil {
			return nil, err // asking again before then only makes it last longer
		}
		c, err := prov.Connect(ctx, acc, set.AccountRegion, p.home)
		a.saveAccountCache(set.AccountProvider, acc) // a new token, even if connecting failed after
		if err != nil {
			return nil, a.noteRateLimit(set.AccountProvider, err)
		}
		a.clearRateLimit(set.AccountProvider)
		p.conn = c
	}
	if set.VpnProvider == "custom" {
		p.customEndpoint = a.resolveThroughTunnel()
	}
	return p, nil
}

// resolveThroughTunnel looks up the imported WireGuard config's server name
// through the running VPN, so switching doesn't ask the system's DNS. With
// no VPN up (first connect) it returns "" and the tunnel resolves the name
// itself with the system's DNS (see PRIVACY.md).
func (a *App) resolveThroughTunnel() string {
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if tun == nil {
		return ""
	}
	data, err := secret.ReadFile(a.customConfPath())
	if err != nil {
		return ""
	}
	cfg, err := tunnel.ParseWGQuick(string(data))
	if err != nil {
		return ""
	}
	host, port, err := net.SplitHostPort(cfg.Endpoint)
	if err != nil || net.ParseIP(host) != nil {
		return "" // already an address
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addrs, err := tun.Net().LookupContextHost(ctx, host)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	pick := addrs[0]
	for _, ad := range addrs { // IPv4 works on more networks
		if ip := net.ParseIP(ad); ip != nil && ip.To4() != nil {
			pick = ad
			break
		}
	}
	return net.JoinHostPort(pick, port)
}

// accountKey identifies a signed-in account, so a connection prepared with
// one account isn't used after the user signed in to another.
func accountKey(acc *vpnprov.Account) string {
	if acc == nil {
		return ""
	}
	// Cached state (a session token) can be refreshed by connecting; that
	// isn't a different account.
	id := map[string]string{}
	for k, v := range acc.Data {
		if !strings.HasPrefix(k, vpnprov.CachePrefix) {
			id[k] = v
		}
	}
	data, _ := json.Marshal(id)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// connectPrepared is connect, optionally with prepareConn already done.
// Callers hold connMu.
func (a *App) connectPrepared(prep *preparedConn) {
	a.mu.Lock()
	if a.eng != nil || a.shuttingDown {
		a.mu.Unlock()
		return
	}
	set := a.settings
	if set.VpnProvider == "none" {
		a.vpn = VpnStatus{State: "connecting", Provider: "none", Name: "No VPN"}
		a.mu.Unlock()
		a.connectDirect(set)
		return
	}
	name := "WARP"
	switch set.VpnProvider {
	case "custom":
		name = "WireGuard"
	case "account":
		name = set.AccountProvider
		if p, ok := vpnprov.Get(set.AccountProvider); ok {
			name = p.Info().Name
		}
	}
	a.vpn = VpnStatus{State: "connecting", Provider: set.VpnProvider, Name: name}
	a.mu.Unlock()
	fail := func(err error) {
		a.setVpn(VpnStatus{State: "error", Provider: set.VpnProvider, Name: name, Error: err.Error()})
	}

	if prep != nil && (prep.set.VpnProvider != set.VpnProvider ||
		prep.set.AccountProvider != set.AccountProvider || prep.set.AccountRegion != set.AccountRegion ||
		prep.account != accountKey(a.account(set.AccountProvider))) {
		prep = nil // settings changed meanwhile
	}
	if prep == nil {
		var err error
		if prep, err = a.prepareConn(set); err != nil {
			fail(err)
			return
		}
	}
	realIP, conn := prep.realIP, prep.conn

	var cfg tunnel.Config
	switch set.VpnProvider {
	case "custom":
		data, err := secret.ReadFile(a.customConfPath())
		if err != nil {
			fail(errors.New("no WireGuard config imported; open Settings to import one"))
			return
		}
		if cfg, err = tunnel.ParseWGQuick(string(data)); err != nil {
			fail(err)
			return
		}
		if prep.customEndpoint != "" {
			cfg.Endpoint = prep.customEndpoint
		}
	case "account":
		cfg = conn.Config
	default:
		acc, err := warp.LoadOrRegister(a.warpPath())
		if err != nil {
			fail(err)
			return
		}
		cfg = warpConfig(acc)
	}

	tun, tr, err := startTunnel(cfg, set.VpnProvider, realIP)
	if err != nil && set.VpnProvider != "account" && set.VpnProvider != "custom" {
		// Cloudflare may have dropped the saved WARP device (rotated endpoint,
		// pruned inactive account). If a fresh registration works, use it.
		if acc, rerr := warp.Reregister(a.warpPath()); rerr == nil {
			runtime.LogWarningf(a.ctx, "%s", engine.Redact(fmt.Sprintf("WARP: saved device failed (%v); registered a new one", err)))
			tun, tr, err = startTunnel(warpConfig(acc), set.VpnProvider, realIP)
		}
	}
	if err != nil {
		fail(err)
		return
	}

	port := 0
	var pfWarning string
	var keepalive func(context.Context) error
	var every time.Duration
	switch {
	case set.VpnProvider == "custom":
		port = set.ForwardedPort
	case conn != nil && conn.PortForward != nil:
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		p, keep, ev, err := conn.PortForward(ctx, tun)
		cancel()
		if err != nil {
			// Not fatal: downloading still works, peers just can't connect in.
			pfWarning = "port forwarding unavailable: " + err.Error()
		} else {
			port, keepalive, every = p, keep, ev
		}
	}
	eng, err := engine.New(tun, engine.Options{
		ForwardedPort: port,
		DownLimit:     set.DownLimitKB * 1024,
		UpLimit:       set.UpLimitKB * 1024,
		StateDir:      a.configDir, // download progress database
	})
	if err != nil {
		tun.Close()
		fail(err)
		return
	}

	st := VpnStatus{State: "connected", Provider: set.VpnProvider, Name: name,
		ExitIP: tr.IP, Location: tr.Location, Colo: tr.Colo, Port: port, Warning: pfWarning}
	if conn != nil {
		st.Server = conn.Server
	}
	mctx := a.startEngine(tun, eng, st)
	go a.monitor(mctx, tun)
	if keepalive != nil {
		go a.keepPortForward(mctx, keepalive, every)
	}
}

// connectDirect starts the engine on the normal network, for "No VPN". It
// only runs when the user picked that; a failing VPN never falls back here.
func (a *App) connectDirect(set Settings) {
	eng, err := engine.New(nil, engine.Options{
		DownLimit: set.DownLimitKB * 1024,
		UpLimit:   set.UpLimitKB * 1024,
		StateDir:  a.configDir,
	})
	if err != nil {
		a.setVpn(VpnStatus{State: "error", Provider: "none", Name: "No VPN", Error: err.Error()})
		return
	}
	a.startEngine(nil, eng, VpnStatus{State: "off", Provider: "none", Name: "No VPN"})
}

// startEngine records the running engine, restores saved and pending
// torrents and starts background work. The returned context ends when the
// engine is stopped.
func (a *App) startEngine(tun *tunnel.Tunnel, eng *engine.Engine, st VpnStatus) context.Context {
	mctx, stop := context.WithCancel(context.Background())
	a.mu.Lock()
	a.tun, a.eng, a.stopMonitor = tun, eng, stop
	a.verifyCtx = mctx
	if tun != nil {
		vpnprov.SetTunnel(tun.DialContext) // server lists and sign-ins go through the VPN
		a.recordEndpoint(tun.Endpoint())
	}
	a.vpn = st
	var saved []savedTorrent
	for i := range a.saved {
		if a.removing[a.saved[i].Hash] {
			continue // its files are being deleted: not to be started again
		}
		saved = append(saved, a.saved[i])
	}
	pending := a.pending
	a.pending = nil
	a.mu.Unlock()

	for _, s := range saved {
		if s.Confirm {
			continue // waiting for the user: not given to the engine yet
		}
		// Adopt only what an older version saved: its files are its own.
		_, err := eng.Add(engine.AddOptions{Source: s.Source, Dir: s.Dir, Skipped: s.Skipped, Paused: s.Paused, Adopt: s.legacy, AcceptWarnings: s.WarningsAccepted})
		if err != nil {
			runtime.LogWarningf(a.ctx, "%s", engine.Redact(fmt.Sprintf("restore %s: %v", s.Hash, err)))
		}
	}
	for _, p := range pending {
		if err := a.addSourceFrom(p.src, p.external); err != nil {
			a.notifyError(err)
		}
	}
	go a.autoCheckUpdates(mctx)
	go a.scanLoop(mctx)
	return mctx
}

func (a *App) warpPath() string { return filepath.Join(a.configDir, "warp.json") }

func warpConfig(acc *warp.Account) tunnel.Config {
	return tunnel.Config{
		PrivateKey:    acc.PrivateKey,
		PeerPublicKey: acc.PeerPublicKey,
		Endpoint:      acc.Endpoint,
		Addresses:     []string{acc.AddressV4, acc.AddressV6},
		DNS:           []string{"1.1.1.1", "1.0.0.1"},
	}
}

// startTunnel brings the tunnel up and confirms traffic leaves through the
// VPN. The engine can only reach the network through the tunnel either way;
// this check is what turns a dead or misrouted tunnel into a clear error.
func startTunnel(cfg tunnel.Config, provider, realIP string) (*tunnel.Tunnel, *tunnel.Trace, error) {
	tun, err := tunnel.Start(cfg)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	tr, err := tun.CheckTrace(ctx)
	cancel()
	switch {
	case err != nil:
		err = fmt.Errorf("VPN not passing traffic: %w", err)
	case provider != "account" && provider != "custom" && tr.Warp != "on" && tr.Warp != "plus":
		err = fmt.Errorf("tunnel is up but Cloudflare reports warp=%q", tr.Warp)
	case tr.IP == "":
		err = errors.New("VPN check failed: Cloudflare's trace page didn't report an exit IP")
	case realIP != "" && tr.IP == realIP:
		err = errors.New("VPN exit IP is the same as your real IP")
	}
	if err != nil {
		tun.Close()
		return nil, nil, err
	}
	return tun, tr, nil
}

// keepPortForward refreshes a provider's port mapping; PIA drops it if not
// renewed every 15 minutes.
func (a *App) keepPortForward(ctx context.Context, keep func(context.Context) error, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := keep(cctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, vpnprov.ErrRenew) {
			// The port's signature is about to expire; only a fresh connection gets a new one.
			a.Reconnect()
			return
		}
		a.mu.Lock()
		if err != nil {
			a.vpn.Warning = "port forwarding refresh failed: " + err.Error()
		} else if strings.HasPrefix(a.vpn.Warning, "port forwarding refresh failed") {
			a.vpn.Warning = ""
		}
		a.mu.Unlock()
	}
}

func (a *App) disconnect() {
	a.mu.Lock()
	eng, tun, stop := a.eng, a.tun, a.stopMonitor
	a.eng, a.tun, a.stopMonitor = nil, nil, nil
	a.mu.Unlock()
	vpnprov.SetTunnel(nil)
	if stop != nil {
		stop()
	}
	if eng != nil {
		eng.Close()
	}
	if tun != nil {
		tun.Close()
	}
}

// monitor re-checks the exit IP every minute so the UI reflects reality.
func (a *App) monitor(ctx context.Context, tun *tunnel.Tunnel) {
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		tr, err := tun.CheckTrace(cctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		a.mu.Lock()
		if err != nil {
			a.vpn.Error = "last check failed: " + err.Error()
		} else {
			a.vpn.ExitIP, a.vpn.Location, a.vpn.Colo, a.vpn.Error = tr.IP, tr.Location, tr.Colo, ""
		}
		a.mu.Unlock()
	}
}

func (a *App) engine() (*engine.Engine, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.eng == nil {
		return nil, errors.New("VPN not connected yet")
	}
	return a.eng, nil
}

// --- Torrents (bound to the frontend) ---

func (a *App) GetVpnStatus() VpnStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.vpn
}

// Reconnect tears down the VPN and engine and starts them again with the
// current settings. Torrents resume where they left off.
func (a *App) Reconnect() {
	gen := a.reconnectGen.Add(1)
	go func() {
		a.connMu.Lock()
		defer a.connMu.Unlock()
		superseded := func() bool { return a.reconnectGen.Load() != gen }
		if superseded() {
			return // a newer Reconnect is queued and will do it
		}
		// Talk to the (new) provider's API through the current tunnel while
		// it's still up, instead of over the normal connection after closing
		// it. If that fails (the old tunnel may be why we're reconnecting),
		// connect prepares again on its own.
		a.mu.Lock()
		set, hadTunnel := a.settings, a.tun != nil
		a.mu.Unlock()
		var prep *preparedConn
		if hadTunnel && set.VpnProvider != "none" {
			a.mu.Lock()
			before := a.vpn
			a.vpn.State, a.vpn.Error = "connecting", "" // switching; torrents keep running meanwhile
			a.mu.Unlock()
			var err error
			prep, err = a.prepareConn(set)
			if superseded() || a.isShuttingDown() {
				// The settings changed again while preparing (leave the old
				// tunnel up so the newer Reconnect prepares through it too),
				// or the app is quitting. Put the real status back first, so
				// the next Reconnect saves that rather than "connecting".
				a.mu.Lock()
				a.vpn = before
				a.mu.Unlock()
				return
			}
			if err != nil && a.tunnelWorks() {
				// Don't close a working VPN and then contact the new
				// provider over the normal connection instead: stay on the
				// current VPN and say why the switch didn't happen.
				a.mu.Lock()
				a.vpn = before
				a.vpn.Warning = "couldn't switch VPN through the current connection: " + err.Error() + " (still connected as before; Retry, or restart the app to connect directly)"
				a.mu.Unlock()
				return
			}
			// If preparing failed because the current tunnel is dead, there's
			// no working VPN to protect: connect like the first time (prep is
			// nil, so connecting prepares again once the tunnel is closed).
		} else if a.isShuttingDown() {
			return
		}
		a.disconnect()
		a.connectPrepared(prep)
	}()
}

// tunnelWorks reports whether the current tunnel still passes traffic.
func (a *App) tunnelWorks() bool {
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if tun == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tr, err := tun.CheckTrace(ctx)
	return err == nil && tr.IP != ""
}

func (a *App) ListTorrents() []engine.Status {
	var list []engine.Status
	if eng, err := a.engine(); err == nil {
		list = eng.List()
	}
	a.mu.Lock()
	kept := list[:0]
	for _, t := range list {
		if !a.removing[t.Hash] {
			kept = append(kept, t)
		}
	}
	list = kept
	a.mu.Unlock()
	// Torrents added from outside the app, waiting for the user.
	a.mu.Lock()
	for _, s := range a.saved {
		if s.Confirm {
			list = append(list, engine.Status{Hash: s.Hash, Name: scan.StripBidiControls(s.Name), Size: s.Size, State: "paused", Warnings: s.Warnings})
		}
	}
	a.mu.Unlock()
	if list == nil {
		return []engine.Status{}
	}
	return a.withScans(list)
}

func (a *App) AddMagnet(link string) error {
	link = strings.TrimSpace(link)
	if !strings.HasPrefix(link, "magnet:") {
		return errors.New("not a magnet link")
	}
	return a.addSource(link)
}

// PickTorrentFile opens a file dialog and adds the chosen .torrent files.
func (a *App) PickTorrentFile() error {
	paths, err := runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Add torrent files",
		Filters: []runtime.FileFilter{{DisplayName: "Torrent files (*.torrent)", Pattern: "*.torrent"}},
	})
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := a.addSource(p); err != nil {
			return err
		}
	}
	return nil
}

// addSource adds a magnet link or .torrent path into the current download folder.
func (a *App) addSource(src string) error { return a.addSourceFrom(src, false) }

// addSourceFrom adds a torrent; an external one waits paused for the user
// (savedTorrent.Confirm).
func (a *App) addSourceFrom(src string, external bool) error {
	var pending savedTorrent // what an external source says about itself
	if strings.HasPrefix(src, "magnet:") {
		m, err := metainfo.ParseMagnetUri(src)
		if err != nil {
			return err
		}
		pending = savedTorrent{Hash: m.InfoHash.HexString(), Name: m.DisplayName}
	} else {
		mi, err := metainfo.LoadFromFile(src)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(src), err)
		}
		info, err := mi.UnmarshalInfo()
		if err != nil {
			return fmt.Errorf("%s: not a valid .torrent file: %w", filepath.Base(src), err)
		}
		var files []scan.File
		top := ""
		if info.IsDir() {
			top = info.BestName() // multi-file paths don't include it
		}
		for _, f := range info.UpvertedFiles() {
			files = append(files, scan.File{Path: f.DisplayPath(&info), Size: f.Length, Top: top})
		}
		pending = savedTorrent{Hash: mi.HashInfoBytes().HexString(), Name: info.BestName(), Size: info.TotalLength(), Warnings: scan.Inspect(files)}
		// Keep our own copy so the torrent survives the original being deleted.
		dst := filepath.Join(a.configDir, "torrents", mi.HashInfoBytes().HexString()+".torrent")
		if !strings.EqualFold(src, dst) {
			if err := copyFile(src, dst); err != nil {
				return err
			}
		}
		src = dst
	}
	a.mu.Lock()
	dir := a.settings.DownloadDir
	a.mu.Unlock()
	if external {
		// Only remembered until the user presses Start (StartTorrent).
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, s := range a.saved {
			if s.Hash == pending.Hash {
				return nil
			}
		}
		pending.Source, pending.Dir, pending.Confirm = src, dir, true
		if pending.Name == "" {
			pending.Name = pending.Hash
		}
		a.saved = append(a.saved, pending)
		return a.writeSaved()
	}
	eng, err := a.engine()
	if err != nil {
		return err
	}
	if err := a.notBeingRemoved(pending.Hash); err != nil {
		return err
	}
	if err := ensureDownloadDir(dir); err != nil {
		return err
	}
	hash, err := eng.Add(engine.AddOptions{Source: src, Dir: dir})
	if err != nil {
		return err
	}
	return a.recordAdded(hash, src, dir)
}

// ensureDownloadDir makes sure dir exists to download into. It creates dir
// itself, but not missing folders above it: those mean a drive that isn't
// connected (a network drive's /Volumes/X on macOS, a drive letter on
// Windows), and creating them would put the downloads on the wrong disk. On
// Linux an unmounted drive leaves an empty folder that looks like any
// other, so a drive in /etc/fstab that isn't mounted is caught too.
func ensureDownloadDir(dir string) error {
	notConnected := func(what string) error {
		return fmt.Errorf("the download folder %s isn't available: %s. If it's on a network drive or another disk, connect it, or choose another folder in Settings", dir, what)
	}
	// First, even if the folder exists: the download folder can be the
	// mount point itself, an existing empty folder while the drive is away.
	if mp := platform.UnmountedDrive(dir); mp != "" {
		return notConnected("the drive at " + mp + " isn't mounted")
	}
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("the download folder %s is a file, not a folder", dir)
		}
		return nil
	}
	parent := filepath.Dir(dir)
	mkdir := os.Mkdir
	if _, err := os.Stat(parent); err != nil {
		// Inside the home folder a missing parent is just a folder that
		// isn't there yet (no ~/Downloads, the default's parent, on a
		// minimal Linux): create it. Elsewhere (a drive letter, /Volumes,
		// /mnt) it's likely a drive that isn't connected.
		if !insideHome(dir) {
			return notConnected(parent + " doesn't exist")
		}
		mkdir = os.MkdirAll
	}
	if err := mkdir(dir, 0o755); err != nil {
		// A drive that's set to connect when used but can't (a NAS that's
		// off) refuses with "operation not permitted", say.
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err // the path is in the message already
		}
		return notConnected("it couldn't be created (" + err.Error() + ")")
	}
	return nil
}

// userHome is a var so tests can use a folder of their own.
var userHome = os.UserHomeDir

// insideHome reports whether dir is in (not equal to) the user's home folder.
func insideHome(dir string) bool {
	home, err := userHome()
	if err != nil || home == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(dir))
	return err == nil && rel != "." && filepath.IsLocal(rel)
}

// recordAdded saves a torrent the engine has just been given from inside the
// app (checked: never adopted as an older version's).
func (a *App) recordAdded(hash, src, dir string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.saved {
		if a.saved[i].Hash == hash {
			// Waiting from an outside add, now added from inside the app: the
			// engine has it as just added (in dir, from src, checked). An
			// entry already running keeps its own folder, as the engine does.
			if s := &a.saved[i]; s.Confirm {
				s.Confirm, s.Checked, s.legacy, s.Dir, s.Source, s.Name, s.Size = false, true, false, dir, src, "", 0
				return a.writeSaved()
			}
			return nil
		}
	}
	a.saved = append(a.saved, savedTorrent{Hash: hash, Source: src, Dir: dir, Checked: true})
	return a.writeSaved()
}

// pendingFor returns a torrent added from outside the app that's waiting
// for the user, nil if hash isn't one. Call with a.mu held.
func (a *App) pendingFor(hash string) *savedTorrent {
	if s := a.savedFor(hash); s != nil && s.Confirm {
		return s
	}
	return nil
}

// StartTorrent starts a torrent that was added from outside the app: only
// now does the engine get it. Called from the window, where the banner
// showed its warnings.
func (a *App) StartTorrent(hash string) error { return a.startTorrent(hash, true) }

// startTorrent starts a waiting torrent. sawWarnings: the user was shown
// its warnings (the window's banner; not the phone page), so starting it
// accepts them; without that, a torrent with warnings isn't started.
func (a *App) startTorrent(hash string, sawWarnings bool) error {
	a.mu.Lock()
	s := a.pendingFor(hash)
	var src, dir string
	var accepted bool
	if s != nil {
		src, dir, accepted = s.Source, s.Dir, len(s.Warnings) > 0
	}
	a.mu.Unlock()
	if s == nil {
		return a.setPaused(hash, false, sawWarnings)
	}
	if accepted && !sawWarnings {
		return engine.ErrWarnings
	}
	eng, err := a.engine()
	if err != nil {
		return err
	}
	if err := a.notBeingRemoved(hash); err != nil {
		return err
	}
	if err := ensureDownloadDir(dir); err != nil {
		return err
	}
	if _, err := eng.Add(engine.AddOptions{Source: src, Dir: dir, AcceptWarnings: accepted}); err != nil {
		return err
	}
	a.updateSaved(hash, func(s *savedTorrent) {
		s.Confirm, s.Paused, s.Name, s.Size, s.Checked = false, false, "", 0, true
		s.WarningsAccepted = s.WarningsAccepted || accepted
	})
	return nil
}

func (a *App) Pause(hash string) error { return a.setPaused(hash, true, false) }

// Resume is the window's Resume, where a torrent's warnings are shown:
// resuming one accepts the warnings shown (their number, as displayed;
// see engine.ResumeAccepting).
func (a *App) Resume(hash string, shownWarnings int) error {
	return a.setPausedShown(hash, false, true, shownWarnings)
}

// resumeRemote is the phone's Resume: the phone page doesn't show warnings,
// so a torrent with warnings the user hasn't accepted isn't resumed.
func (a *App) resumeRemote(hash string) error { return a.setPaused(hash, false, false) }

func (a *App) setPaused(hash string, paused, sawWarnings bool) error {
	return a.setPausedShown(hash, paused, sawWarnings, -1)
}

// setPausedShown: shown is how many warnings the window displayed for it
// (-1: unknown, from a Start in the outside-add banner, which showed them).
func (a *App) setPausedShown(hash string, paused, sawWarnings bool, shown int) error {
	a.mu.Lock()
	waiting := a.pendingFor(hash) != nil
	a.mu.Unlock()
	if waiting {
		if paused {
			return nil // not running anyway
		}
		return a.startTorrent(hash, sawWarnings) // Resume (here or from the phone) starts it
	}
	eng, err := a.engine()
	if err != nil {
		return err
	}
	accepted := false
	switch {
	case paused:
		err = eng.Pause(hash)
	case sawWarnings && shown >= 0:
		accepted, err = eng.ResumeAccepting(hash, shown)
	default:
		err = eng.Resume(hash) // engine.ErrWarnings if they're not accepted
	}
	if err != nil {
		return err
	}
	a.updateSaved(hash, func(s *savedTorrent) {
		s.Paused = paused
		if !paused {
			s.Confirm = false // resumed: the user has accepted it
		}
		if accepted {
			s.WarningsAccepted = true
		}
	})
	return nil
}

func (a *App) PauseAll() {
	for _, t := range a.ListTorrents() {
		if t.State != "paused" {
			a.Pause(t.Hash)
		}
	}
}

func (a *App) ResumeAll() {
	for _, t := range a.ListTorrents() {
		// Not one added from outside (the user starts those), nor one
		// with warnings not yet accepted: those one at a time, in the window.
		if t.State == "paused" && !t.Confirm && !t.Held {
			a.setPaused(t.Hash, false, false)
		}
	}
}

// Remove drops a torrent from the list, optionally deleting its data.
func (a *App) Remove(hash string, deleteFiles bool) error {
	a.mu.Lock()
	waiting := a.pendingFor(hash) != nil
	a.mu.Unlock()
	if waiting {
		return a.forget(hash) // never given to the engine: no files either
	}
	eng, err := a.engine()
	if err != nil {
		return err
	}
	// Reserved first, at app level (it survives a VPN reconnect replacing
	// the engine): the torrent leaves the list, no new work on its files
	// starts, and it can't be added again until its files are gone.
	a.mu.Lock()
	if a.removing[hash] {
		a.mu.Unlock()
		return nil // already being removed
	}
	if a.removing == nil {
		a.removing = map[string]bool{}
	}
	a.removing[hash] = true
	a.mu.Unlock()
	// Deleting files can take a while (waiting for the torrent's files to
	// close, a slow network share), so the list doesn't wait for it: past a
	// moment, Remove returns and any problem is reported when it's done.
	done := make(chan error, 1)
	go func() {
		a.stopWork(hash, 10*time.Second) // a scan, .sfv check or Verify reading its files
		err := eng.Remove(hash, deleteFiles)
		// Forgotten before the reservation ends, so a VPN reconnect can't
		// start it again in between. Refused because its drive is away:
		// it stays, untouched.
		if !errors.Is(err, engine.ErrFolderUnavailable) {
			a.forget(hash)
		}
		a.mu.Lock()
		delete(a.removing, hash)
		a.mu.Unlock()
		done <- err
	}()
	var rmErr error
	select {
	case rmErr = <-done:
	case <-time.After(1500 * time.Millisecond):
		go func() {
			if err := <-done; err != nil && !errors.Is(err, engine.ErrNotFound) {
				a.notifyError(removeError(err))
			}
		}()
		return nil // the goroutine above forgets it (or keeps it, if refused)
	}
	// The goroutine has already forgotten the torrent, unless the remove
	// was refused because its drive is away: then it stays, untouched.
	if errors.Is(rmErr, engine.ErrNotFound) {
		return nil // already removed (a second click while the first finished)
	}
	if rmErr != nil {
		return removeError(rmErr)
	}
	return nil
}

// removeError says what happened: refused (nothing changed, the torrent
// stays) or removed with some files left behind.
func removeError(err error) error {
	if errors.Is(err, engine.ErrFolderUnavailable) {
		return fmt.Errorf("not removed: %w", err)
	}
	return fmt.Errorf("removed, but some files couldn't be deleted: %w", err)
}

// forget drops a torrent from the saved list, with the app's copy of its
// .torrent file.
func (a *App) forget(hash string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := a.saved[:0]
	for _, s := range a.saved {
		if s.Hash == hash {
			if strings.HasPrefix(s.Source, filepath.Join(a.configDir, "torrents")) {
				os.Remove(s.Source)
			}
			continue
		}
		kept = append(kept, s)
	}
	a.saved = kept
	return a.writeSaved()
}

func (a *App) GetFiles(hash string) ([]engine.FileStatus, error) {
	a.mu.Lock()
	waiting := a.pendingFor(hash) != nil
	a.mu.Unlock()
	if waiting {
		return []engine.FileStatus{}, nil // file list unknown until it's started
	}
	eng, err := a.engine()
	if err != nil {
		return nil, err
	}
	return eng.Files(hash)
}

func (a *App) SetFilesWanted(hash string, indexes []int, wanted bool) error {
	eng, err := a.engine()
	if err != nil {
		return err
	}
	skipped, err := eng.SetFilesWanted(hash, indexes, wanted)
	if err != nil {
		return err
	}
	a.updateSaved(hash, func(s *savedTorrent) { s.Skipped = skipped })
	return nil
}

// OpenFolder opens a torrent's folder in Explorer, or the default download
// folder if hash is empty.
func (a *App) OpenFolder(hash string) error {
	a.mu.Lock()
	dir := a.settings.DownloadDir
	for _, s := range a.saved {
		if s.Hash == hash {
			dir = s.Dir
		}
	}
	a.mu.Unlock()
	// Show the torrent's own folder, or select its file if it's a single
	// file, once its data exists. Otherwise fall back to the download folder.
	if hash != "" {
		if eng, err := a.engine(); err == nil {
			if p, isDir, ok := eng.ContentPath(hash); ok {
				if _, err := os.Stat(p); err == nil {
					return platform.RevealPath(p, isDir)
				}
			}
		}
	}
	if err := ensureDownloadDir(dir); err != nil {
		return err
	}
	return platform.OpenFolder(dir)
}

// --- Settings (bound to the frontend) ---

type SettingsView struct {
	Settings       Settings             `json:"settings"`
	CustomEndpoint string               `json:"customEndpoint"` // "" if no config imported
	Assoc          platform.AssocStatus `json:"assoc"`
	// Accounts maps provider ID to a display name for signed-in accounts.
	Accounts map[string]string `json:"accounts"`
	// Antivirus the app scans finished downloads with, and whether it's installed.
	Scanner          string `json:"scanner"`
	ScannerAvailable bool   `json:"scannerAvailable"`
	// KeyringMissing: Linux without KWallet/GNOME Keyring, so secrets are
	// protected by a private key file only.
	KeyringMissing bool `json:"keyringMissing"`
}

func (a *App) GetSettings() SettingsView {
	// Outside the lock: it may ask the keyring over D-Bus.
	keyringMissing := secret.KeyringMissing()
	a.mu.Lock()
	v := SettingsView{Settings: a.settings, Accounts: map[string]string{}}
	v.Scanner, v.ScannerAvailable = scan.Scanner()
	v.KeyringMissing = keyringMissing
	a.mu.Unlock()
	accs, _ := a.loadAccounts()
	for id, acc := range accs {
		v.Accounts[id] = acc.Display
	}
	if data, err := secret.ReadFile(a.customConfPath()); err == nil {
		if cfg, err := tunnel.ParseWGQuick(string(data)); err == nil {
			v.CustomEndpoint = cfg.Endpoint
		}
	}
	if !storeBuild { // the Store package registers these itself
		v.Assoc = platform.Associations(a.exe)
	}
	return v
}

// SaveSettings persists and applies settings. Speed limits and the download
// folder apply immediately; VPN changes trigger a reconnect.
func (a *App) SaveSettings(s Settings) error {
	if s.DownloadDir == "" {
		return errors.New("download folder can't be empty")
	}
	if s.VpnProvider != "custom" && s.VpnProvider != "account" && s.VpnProvider != "none" {
		s.VpnProvider = "warp"
	}
	if s.VpnProvider == "account" && a.account(s.AccountProvider) == nil {
		return errors.New("sign in to a VPN account before switching to it")
	}
	if s.ForwardedPort < 0 || s.ForwardedPort > 65535 {
		return errors.New("forwarded port must be between 0 and 65535")
	}
	if s.UpdateChannel != channelBeta {
		s.UpdateChannel = channelStable
	}
	if s.DownLimitKB < 0 || s.UpLimitKB < 0 {
		return errors.New("speed limits can't be negative")
	}
	if s.VpnProvider == "custom" {
		if _, err := os.Stat(a.customConfPath()); err != nil {
			return errors.New("import a WireGuard config before switching to it")
		}
	}

	a.mu.Lock()
	old := a.settings
	// The remote control has its own controls (SetRemote); the Settings
	// form's copy may be stale.
	s.RemoteEnabled, s.RemotePort, s.RemoteTailscale = old.RemoteEnabled, old.RemotePort, old.RemoteTailscale
	a.settings = s
	eng := a.eng
	a.mu.Unlock()
	if err := a.writeSettings(); err != nil {
		return err
	}
	if eng != nil {
		eng.SetLimits(s.DownLimitKB*1024, s.UpLimitKB*1024)
	}
	if old.UpdateChannel != s.UpdateChannel && a.ctx != nil {
		// An update offered on the other channel no longer applies.
		runtime.EventsEmit(a.ctx, "update-channel-changed")
	}
	vpnChanged := old.VpnProvider != s.VpnProvider ||
		(s.VpnProvider == "custom" && old.ForwardedPort != s.ForwardedPort) ||
		(s.VpnProvider == "account" && (old.AccountProvider != s.AccountProvider || old.AccountRegion != s.AccountRegion))
	if vpnChanged {
		a.Reconnect()
	}
	return nil
}

// ChooseDirectory shows a folder picker and returns the choice ("" if cancelled).
func (a *App) ChooseDirectory(current string) (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Download folder",
		DefaultDirectory: existingAncestor(current),
	})
}

// existingAncestor returns dir, or its nearest parent that exists, or "" if
// none does. The OS folder picker fails if told to start in a missing folder
// (e.g. the user deleted the download folder).
func existingAncestor(dir string) string {
	for dir != "" {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// ChooseDownloadDir picks and immediately saves a new default download folder.
func (a *App) ChooseDownloadDir() (string, error) {
	a.mu.Lock()
	s := a.settings
	a.mu.Unlock()
	dir, err := a.ChooseDirectory(s.DownloadDir)
	if err != nil || dir == "" {
		return s.DownloadDir, err
	}
	s.DownloadDir = dir
	return dir, a.SaveSettings(s)
}

// ImportWireGuardConfig lets the user pick a .conf file, validates it and
// stores it. Returns the endpoint for display. If the custom provider is
// already active, it reconnects with the new config.
func (a *App) ImportWireGuardConfig() (string, error) {
	path, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Import WireGuard config",
		Filters: []runtime.FileFilter{{DisplayName: "WireGuard config (*.conf)", Pattern: "*.conf"}},
	})
	if err != nil || path == "" {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	cfg, err := tunnel.ParseWGQuick(string(data))
	if err != nil {
		return "", err
	}
	// Stored encrypted; the original file the user picked is left alone.
	if err := secret.WriteFile(a.customConfPath(), data); err != nil {
		return "", err
	}
	a.mu.Lock()
	active := a.settings.VpnProvider == "custom"
	a.mu.Unlock()
	if active {
		a.Reconnect()
	}
	return cfg.Endpoint, nil
}

func (a *App) RegisterAssociations() (platform.AssocStatus, error) {
	if storeBuild {
		return platform.AssocStatus{}, errors.New("the Microsoft Store version registers magnet links and .torrent files when installed")
	}
	if err := platform.RegisterAssociations(a.exe); err != nil {
		return platform.AssocStatus{}, err
	}
	return platform.Associations(a.exe), nil
}

func (a *App) OpenDefaultAppsSettings() error {
	return platform.OpenDefaultAppsSettings()
}

// --- Persistence ---

func (a *App) customConfPath() string { return filepath.Join(a.configDir, "wireguard.conf") }

func (a *App) loadSettings() {
	home, _ := os.UserHomeDir()
	a.settings = Settings{
		DownloadDir:     filepath.Join(home, "Downloads", "Torrents"),
		VpnProvider:     "warp",
		CloseToTray:     true,
		AutoUpdateCheck: true,
		UpdateChannel:   channelStable,
		ScanDownloads:   true,
		AutoVerify:      "suspect",
	}
	if data, err := os.ReadFile(filepath.Join(a.configDir, "settings.json")); err == nil {
		json.Unmarshal(data, &a.settings)
	}
}

func (a *App) writeSettings() error {
	a.mu.Lock()
	data, _ := json.MarshalIndent(a.settings, "", "  ")
	a.mu.Unlock()
	return writeFile(filepath.Join(a.configDir, "settings.json"), data)
}

func (a *App) loadSaved() {
	data, err := os.ReadFile(filepath.Join(a.configDir, "torrents.json"))
	if err != nil {
		return
	}
	json.Unmarshal(data, &a.saved)
	var raw []map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	for i := range a.saved {
		if a.saved[i].Dir == "" { // saved by an older version
			a.saved[i].Dir = a.settings.DownloadDir
		}
		// No "checked" field at all: written by v0.2.36 or older (this
		// version always writes it, except for torrents still waiting to be
		// started, which older versions didn't have).
		if i < len(raw) && !a.saved[i].Confirm {
			_, has := raw[i]["checked"]
			a.saved[i].legacy = !has
		}
	}
}

func (a *App) updateSaved(hash string, f func(*savedTorrent)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.saved {
		if a.saved[i].Hash == hash {
			f(&a.saved[i])
		}
	}
	if err := a.writeSaved(); err != nil {
		runtime.LogErrorf(a.ctx, "%s", engine.Redact(fmt.Sprintf("save torrents: %v", err)))
	}
}

// writeSaved must be called with a.mu held.
func (a *App) writeSaved() error {
	data, _ := json.MarshalIndent(a.saved, "", "  ")
	return writeFile(filepath.Join(a.configDir, "torrents.json"), data)
}

// writeFile writes atomically so a crash can't leave a half-written file.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// GetPieceMap returns a torrent's pieces as runs of states for the piece map.
func (a *App) GetPieceMap(hash string) ([]engine.PieceRun, error) {
	eng, err := a.engine()
	if err != nil {
		return nil, err
	}
	return eng.PieceMap(hash)
}

// recordEndpoint notes the VPN server connected to, only while the release
// leak test (cmd/leakcheck) runs: it creates leakcheck-active, reads the
// servers to allow from vpn-endpoint afterwards, and removes both. Normal
// use keeps no record of which servers were used.
func (a *App) recordEndpoint(ep string) {
	flagFile := filepath.Join(a.configDir, "leakcheck-active")
	fi, err := os.Stat(flagFile)
	if err != nil {
		return
	}
	if time.Since(fi.ModTime()) > 6*time.Hour {
		// Left behind by a check that didn't finish (no run takes this
		// long): not a reason to keep recording servers.
		os.Remove(flagFile)
		os.Remove(filepath.Join(a.configDir, "vpn-endpoint"))
		return
	}
	f, err := os.OpenFile(filepath.Join(a.configDir, "vpn-endpoint"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintln(f, ep)
	f.Close()
}

// notBeingRemoved refuses a torrent whose files are still being deleted:
// adding it now would create files the removal then deletes.
func (a *App) notBeingRemoved(hash string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if hash != "" && a.removing[strings.ToLower(hash)] {
		return errors.New("this torrent is still being removed; try again in a moment")
	}
	return nil
}
