package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sharkbite/internal/engine"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/skip2/go-qrcode"
	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/net/netutil"

	"sharkbite/internal/search"
	"sharkbite/internal/secret"
)

// Remote control: a small web page for phones on the home network, to add
// and manage torrents on this computer. It's off by default.
//
// Security model:
//   - It only listens on real local networks (an interface whose default
//     gateway has a hardware address: Wi-Fi or Ethernet, not Docker/WSL/
//     Hyper-V adapters or overlay VPNs), never inside the app's VPN tunnel,
//     and only answers private addresses.
//   - Once a phone has paired, it only runs on that home network (same
//     router), so a laptop on café or hotel Wi-Fi doesn't serve it there.
//   - The Host header must be an IP address or "localhost" (no DNS
//     rebinding). Pairing must be a JSON POST, which a web page can't send
//     cross-site without a CORS preflight we never answer.
//   - A phone pairs by scanning a QR code or typing an 8-character code (one
//     use, 5 minutes) and gets a random access key, sent as an Authorization
//     header; keys are stored only as hashes, encrypted. A valid key is never
//     rate-limited; wrong keys and wrong codes are, separately.
//   - Phones get only what the page shows: no VPN exit IP, forwarded port or
//     download folders.
//   - Plain HTTP: the owner chose this for the home network. Someone else on
//     the Wi-Fi could read the traffic (torrent names, searches, the key).

//go:embed remote/*
var remoteFiles embed.FS

const (
	defaultRemotePort = 8732
	pairCodeLifetime  = 5 * time.Minute
	maxTorrentUpload  = 10 << 20
	maxRemoteConns    = 64
	maxConnsPerDevice = 16 // so one device can't take every slot
	recheckNetwork    = 20 * time.Second
)

type remoteDevice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IP   string `json:"ip"` // where it paired from
	// ViaTailscale: paired over Tailscale (then IP is its Tailscale address
	// and TSName its Tailscale name). Only home-Wi-Fi pairings set the home
	// network.
	ViaTailscale bool   `json:"viaTailscale"`
	TSName       string `json:"tsName,omitempty"`
	TokenHash    string `json:"tokenHash"`
	Created      int64  `json:"created"`
	LastSeen     int64  `json:"lastSeen"`
}

// remoteStore is remote.json (encrypted): the home network and the paired
// phones. It's loaded once and kept in memory.
type remoteStore struct {
	Home    *lanNet        `json:"home"`
	Devices []remoteDevice `json:"devices"`
}

type remoteServer struct {
	srv    *http.Server
	port   int
	addrs  []string // local addresses it listens on
	tsAddr string   // the Tailscale one among them, if Tailscale access is on
	// whois says who a Tailscale address is (tailscaleWhois; replaceable in tests).
	whois func(ip string) tailscalePeer

	mu           sync.Mutex
	pairCode     string
	pairUntil    time.Time
	pairFailures int // wrong codes against the current one, from anywhere
	keyFails     map[string][]time.Time
	codeFails    map[string][]time.Time

	// Search results by ID. Their download links point at the user's
	// Prowlarr/Jackett and contain its API key, so they stay on this
	// computer: the phone only gets an ID, a title and numbers, and sends
	// back the ID (so it also can't make the app fetch a URL of its choice).
	results      map[string]search.Result
	resultsSince map[string]time.Time
}

// phoneResult is a search result as the phone sees it.
type phoneResult struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Size      int64  `json:"size"`
	Seeders   int    `json:"seeders"`
	Leechers  int    `json:"leechers"`
	Indexer   string `json:"indexer"`
	Published string `json:"published"`
}

const (
	resultLifetime = 30 * time.Minute
	maxResults     = 2000
)

// keepResults stores results for later and returns what the phone may see.
func (rs *remoteServer) keepResults(res []search.Result) []phoneResult {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.results == nil {
		rs.results, rs.resultsSince = map[string]search.Result{}, map[string]time.Time{}
	}
	now := time.Now()
	for id, t := range rs.resultsSince {
		if now.Sub(t) > resultLifetime || len(rs.results)+len(res) > maxResults {
			delete(rs.results, id)
			delete(rs.resultsSince, id)
		}
	}
	out := make([]phoneResult, 0, len(res))
	for _, r := range res {
		id := randomString(9)
		rs.results[id], rs.resultsSince[id] = r, now
		out = append(out, phoneResult{id, r.Title, r.Size, r.Seeders, r.Leechers, r.Indexer, r.Published})
	}
	return out
}

func (rs *remoteServer) result(id string) (search.Result, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r, ok := rs.results[id]
	if ok && time.Since(rs.resultsSince[id]) > resultLifetime {
		return search.Result{}, false
	}
	return r, ok
}

// RemoteStatus is what Settings shows.
type RemoteStatus struct {
	Enabled bool   `json:"enabled"`
	Running bool   `json:"running"`
	Paused  string `json:"paused"` // why it isn't running (on the home network) although enabled
	// Tailscale access (opt-in): Tailscale is the setting; TailscaleURL is
	// set while it's listening there; TailscaleFound says whether Tailscale
	// is installed and connected on this computer.
	Tailscale bool `json:"tailscale"`
	// LanAvailable: a home-Wi-Fi pairing is possible here (the computer is
	// on the home network, or no home network is set and it\'s on one).
	LanAvailable   bool               `json:"lanAvailable"`
	TailscaleURL   string             `json:"tailscaleURL"`
	TailscaleFound bool               `json:"tailscaleFound"`
	Home           string             `json:"home"` // the home network's router, if paired
	Port           int                `json:"port"`
	URLs           []string           `json:"urls"`
	Error          string             `json:"error"`
	Devices        []RemoteDeviceView `json:"devices"`
}

type RemoteDeviceView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	IP           string `json:"ip"`
	ViaTailscale bool   `json:"viaTailscale"`
	TSName       string `json:"tsName"`
	Created      int64  `json:"created"`
	LastSeen     int64  `json:"lastSeen"`
}

// RemotePairing is shown as a QR code in Settings.
type RemotePairing struct {
	URL     string `json:"url"`
	Code    string `json:"code"` // to type on the phone instead of scanning, e.g. "K7QM-4XRP"
	QR      string `json:"qr"`   // data: URL of a PNG
	Expires int64  `json:"expires"`
}

func (a *App) remotePath() string { return filepath.Join(a.configDir, "remote.json") }

// loadRemoteStore reads remote.json once. If it can't be read (e.g. a
// locked keyring), the remote control stays off rather than starting with
// no phones and then overwriting the file.
func (a *App) loadRemoteStore() {
	a.remoteMu.Lock()
	defer a.remoteMu.Unlock()
	a.store, a.storeErr = remoteStore{}, nil
	data, err := secret.ReadFile(a.remotePath())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		a.storeErr = fmt.Errorf("couldn't read the paired phones (%v)", err)
		return
	}
	if len(data) > 0 && data[0] == '[' { // older format: just the devices
		err = json.Unmarshal(data, &a.store.Devices)
	} else {
		err = json.Unmarshal(data, &a.store)
	}
	if err != nil {
		a.storeErr = fmt.Errorf("couldn't read the paired phones (%v)", err)
		return
	}
	// Pairings from before devices were tagged: one that came from a
	// Tailscale address was paired over Tailscale.
	for i := range a.store.Devices {
		if d := &a.store.Devices[i]; !d.ViaTailscale && isTailscaleAddr(net.ParseIP(d.IP)) {
			d.ViaTailscale = true
		}
	}
}

// saveRemoteStore writes remote.json. Call with remoteMu held.
func (a *App) saveRemoteStore() error {
	if a.storeErr != nil {
		return a.storeErr
	}
	data, _ := json.Marshal(a.store)
	return secret.WriteFile(a.remotePath(), data)
}

func (a *App) remotePort() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.settings.RemotePort > 0 {
		return a.settings.RemotePort
	}
	return defaultRemotePort
}

// remoteAddrs picks the local addresses to listen on now: the home network
// once one is set, otherwise every real local network. reason says why
// there are none.
// With Tailscale access on, the Tailscale address is added (tsAddr): it
// works wherever the computer is, so it isn't tied to the home network.
func (a *App) remoteAddrs() (addrs []string, tsAddr, reason string) {
	lans := lanNetworks()
	a.remoteMu.Lock()
	home := a.store.Home
	a.remoteMu.Unlock()
	// With no home network yet, the home-Wi-Fi listener only runs while a
	// home-Wi-Fi pairing is open (5 minutes): before any phone pairs there,
	// it isn't known which network is home, and it mustn't serve whatever
	// network a laptop happens to be on.
	a.mu.Lock()
	window := time.Now().Before(a.lanPairUntil)
	a.mu.Unlock()
	for _, l := range lans {
		if (home != nil && l.same(*home)) || (home == nil && window) {
			addrs = append(addrs, l.LocalIP)
		}
	}
	lanReason := ""
	switch {
	case len(addrs) > 0, home == nil && len(lans) > 0:
		// listening, or simply waiting for a first home-Wi-Fi pairing
	case home != nil:
		lanReason = "paused on this network: it isn't the home network the phones were paired on (router " + home.Gateway + ")"
	default:
		lanReason = "paused: no home network (Wi-Fi or Ethernet with a router) found"
	}
	a.mu.Lock()
	useTS := a.settings.RemoteTailscale
	a.mu.Unlock()
	if useTS {
		if tsAddr = tailscaleIP(); tsAddr != "" {
			addrs = append(addrs, tsAddr)
		}
	}
	// Said even while Tailscale keeps it running away from home.
	return addrs, tsAddr, lanReason
}

// startRemote starts the remote-control server if it's turned on and the
// computer is on its network. Call with remoteOpMu held.
func (a *App) startRemote() {
	a.mu.Lock()
	enabled := a.settings.RemoteEnabled
	a.mu.Unlock()
	a.remoteMu.Lock()
	storeErr := a.storeErr
	a.remoteMu.Unlock()
	setState := func(err, paused string) {
		a.mu.Lock()
		a.remoteErr, a.remotePaused = err, paused
		a.mu.Unlock()
	}
	if !enabled {
		setState("", "")
		return
	}
	if storeErr != nil {
		setState(storeErr.Error()+"; the remote control stays off", "")
		return
	}
	// Phones paired before home networks were recorded: the computer's
	// current network becomes home (if it's on exactly one), once.
	a.remoteMu.Lock()
	lanPaired := false
	for _, d := range a.store.Devices {
		lanPaired = lanPaired || !d.ViaTailscale
	}
	if a.store.Home == nil && lanPaired {
		if lans := lanNetworks(); len(lans) == 1 {
			a.store.Home = &lans[0]
			a.saveRemoteStore()
		}
	}
	a.remoteMu.Unlock()
	addrs, tsAddr, reason := a.remoteAddrs()
	if len(addrs) == 0 {
		setState("", reason)
		return
	}
	port := a.remotePort()
	rs := &remoteServer{port: port, addrs: addrs, tsAddr: tsAddr, whois: tailscaleWhois, keyFails: map[string][]time.Time{}, codeFails: map[string][]time.Time{}}
	rs.srv = &http.Server{
		Handler:           a.remoteHandler(rs),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second, // searches take up to 90 s
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	var lns []net.Listener
	for _, ip := range addrs {
		// This computer's own network stack: the VPN tunnel is a separate,
		// in-process stack the OS never sees, so this can't be reached
		// through it.
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			setState(fmt.Sprintf("couldn't start on port %d: %v", port, err), "")
			return
		}
		lns = append(lns, netutil.LimitListener(&perDeviceListener{Listener: ln, open: map[string]int{}}, maxRemoteConns))
	}
	a.mu.Lock()
	a.remote, a.remoteErr, a.remotePaused = rs, "", reason
	a.mu.Unlock()
	for _, ln := range lns {
		go func(ln net.Listener) {
			if err := rs.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && a.ctx != nil {
				runtime.LogWarningf(a.ctx, "%s", engine.Redact(fmt.Sprintf("remote control: %v", err)))
			}
		}(ln)
	}
}

// stopRemote stops the server and drops every connection, including
// requests still running. Call with remoteOpMu held.
func (a *App) stopRemote() {
	a.mu.Lock()
	rs := a.remote
	a.remote = nil
	a.mu.Unlock()
	if rs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if rs.srv.Shutdown(ctx) != nil {
			rs.srv.Close()
		}
		cancel()
	}
}

// watchRemoteNetwork follows network changes: leaving the home network
// pauses the server, coming back (or a new local address) restarts it.
func (a *App) watchRemoteNetwork() {
	// NewTicker, not time.Tick: Tick's ticker is never collected, and this
	// loop returns when the app shuts down.
	tick := time.NewTicker(recheckNetwork)
	defer tick.Stop()
	for range tick.C {
		if a.isShuttingDown() {
			return
		}
		a.mu.Lock()
		enabled, rs := a.settings.RemoteEnabled, a.remote
		a.mu.Unlock()
		if !enabled {
			continue
		}
		want, _, _ := a.remoteAddrs()
		var have []string
		if rs != nil {
			have = rs.addrs
		}
		if strings.Join(want, ",") != strings.Join(have, ",") {
			a.remoteOpMu.Lock()
			a.stopRemote()
			a.startRemote()
			a.remoteOpMu.Unlock()
		}
	}
}

// --- request checks ---

// privateClient reports whether a request comes from a private address.
func privateClient(remoteAddr string) (string, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return "", false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host, false
	}
	return ip.String(), ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// allowedClient: on the Tailscale address only Tailscale devices; on the
// home network address only private addresses (as privateClient).
func (rs *remoteServer) allowedClient(r *http.Request) bool {
	local := ""
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		local, _, _ = net.SplitHostPort(la.String())
	}
	if rs.tsAddr != "" && local == rs.tsAddr {
		// The source address alone proves nothing (some networks use
		// 100.64/10 too): Tailscale must know the device, and it must be
		// signed in to the same account as this computer.
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !isTailscaleAddr(net.ParseIP(host)) || rs.whois == nil {
			return false
		}
		return rs.whois(net.ParseIP(host).String()).OK
	}
	_, ok := privateClient(r.RemoteAddr)
	return ok
}

// limitKey groups addresses for rate limiting: IPv6 per /64, since one
// device can use many addresses in its /64.
func limitKey(ip string) string {
	p := net.ParseIP(ip)
	if p != nil && p.To4() == nil {
		return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip
}

// hostIsAddress reports whether the Host header is an IP address or
// localhost: a browser only sends a domain name here if a page tricked it
// into talking to us under that name (DNS rebinding).
func hostIsAddress(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	return h == "localhost" || net.ParseIP(h) != nil
}

// tooMany reports whether key has 10 failures in the last 10 minutes.
func (rs *remoteServer) tooMany(m map[string][]time.Time, key string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	cut := time.Now().Add(-10 * time.Minute)
	recent := m[key][:0]
	for _, t := range m[key] {
		if t.After(cut) {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(m, key)
		return false
	}
	m[key] = recent
	return len(recent) >= 10
}

func (rs *remoteServer) fail(m map[string][]time.Time, key string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(m) > 4096 { // bounded memory: forget old entries wholesale
		for k := range m {
			delete(m, k)
		}
	}
	m[key] = append(m[key], time.Now())
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Pairing codes are 8 characters a person can type: no 0/O, 1/I/L.
// 31^8 ≈ 8.5e11 possibilities against at most 10 guesses per address and 20
// in total before the code (valid 5 minutes) is cancelled.
const pairAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

func newPairCode() string {
	b := make([]byte, 8)
	for i := 0; i < len(b); {
		var c [1]byte
		rand.Read(c[:])
		if int(c[0]) < 256-256%len(pairAlphabet) { // no modulo bias
			b[i] = pairAlphabet[int(c[0])%len(pairAlphabet)]
			i++
		}
	}
	return string(b)
}

// normalizePairCode accepts what people type: lower case, spaces, dashes.
func normalizePairCode(s string) string {
	var out []byte
	for _, c := range strings.ToUpper(s) {
		if c < 128 && strings.ContainsRune(pairAlphabet, c) {
			out = append(out, byte(c))
		}
	}
	return string(out)
}

func randomString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// cleanName keeps a phone's name to 40 printable characters (no control or
// direction-changing characters that could make it look like another).
func cleanName(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if n == 40 {
			break
		}
		if unicode.IsPrint(r) && !unicode.Is(unicode.Bidi_Control, r) {
			b.WriteRune(r)
			n++
		}
	}
	if b.Len() == 0 {
		return "Phone"
	}
	return b.String()
}

// device returns the paired device for the request's access key.
func (a *App) device(r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	want := hashToken(token)
	a.remoteMu.Lock()
	defer a.remoteMu.Unlock()
	for i := range a.store.Devices {
		d := &a.store.Devices[i]
		if subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(want)) == 1 {
			// Note when it was last used; written to disk at most every 10 minutes.
			now := time.Now().Unix()
			if now-d.LastSeen > 600 {
				d.LastSeen = now
				a.saveRemoteStore()
			} else if now-d.LastSeen > 60 {
				d.LastSeen = now
			}
			return d.ID, true
		}
	}
	return "", false
}

// --- the phone's view ---

type phoneVPN struct {
	State    string `json:"state"`
	Name     string `json:"name"`
	Location string `json:"location"`
	Error    string `json:"error"`
}

type phoneTorrent struct {
	Hash     string  `json:"hash"`
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Size     int64   `json:"size"`
	DownRate int64   `json:"downRate"`
	UpRate   int64   `json:"upRate"`
}

// phoneState is only what the phone page shows. The traffic is plain HTTP,
// so the VPN's exit IP, forwarded port and download folders stay off it.
func (a *App) phoneState() any {
	v := a.GetVpnStatus()
	ts := []phoneTorrent{}
	for _, t := range a.ListTorrents() {
		ts = append(ts, phoneTorrent{t.Hash, t.Name, t.State, t.Progress, t.Size, t.DownRate, t.UpRate})
	}
	return map[string]any{"vpn": phoneVPN{v.State, v.Name, v.Location, v.Error}, "torrents": ts}
}

// --- handlers ---

var hashPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (a *App) remoteHandler(rs *remoteServer) http.Handler {
	mux := http.NewServeMux()
	page, _ := fs.Sub(remoteFiles, "remote")
	mux.Handle("GET /", http.FileServer(http.FS(page)))

	mux.HandleFunc("POST /api/pair", func(w http.ResponseWriter, r *http.Request) {
		ip, _ := privateClient(r.RemoteAddr)
		key := limitKey(ip)
		if rs.tooMany(rs.codeFails, key) {
			http.Error(w, "too many wrong codes; try again in a few minutes", http.StatusTooManyRequests)
			return
		}
		// JSON only: a web page can't send that cross-site without a CORS
		// preflight, which this server never answers.
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "bad request", http.StatusUnsupportedMediaType)
			return
		}
		var req struct{ Code, Name string }
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		code := normalizePairCode(req.Code)
		cancelled := false
		rs.mu.Lock()
		ok := rs.pairCode != "" && time.Now().Before(rs.pairUntil) &&
			subtle.ConstantTimeCompare([]byte(code), []byte(rs.pairCode)) == 1
		if ok {
			rs.pairCode = "" // one use only
		} else if rs.pairCode != "" {
			// Besides the per-address limit: too many wrong guesses from
			// anywhere cancel the code altogether.
			if rs.pairFailures++; rs.pairFailures >= 20 {
				rs.pairCode = ""
				cancelled = true
			}
		}
		rs.mu.Unlock()
		if cancelled && a.ctx != nil {
			runtime.EventsEmit(a.ctx, "notice", "Phone pairing was cancelled after too many wrong codes from your network. If you're pairing a phone, show a new code in Settings → Remote control.")
		}
		if !ok {
			rs.fail(rs.codeFails, key)
			http.Error(w, "this pairing code is wrong or expired; show a new one in Settings → Remote control", http.StatusForbidden)
			return
		}

		// A home-Wi-Fi pairing makes its network the home network; a
		// Tailscale pairing doesn't (it says nothing about which LAN is home).
		var here *lanNet
		viaTS, tsName := false, ""
		if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			if host, _, err := net.SplitHostPort(la.String()); err == nil {
				if rs.tsAddr != "" && host == rs.tsAddr {
					viaTS = true
					if rs.whois != nil {
						tsName = rs.whois(ip).Name
					}
				} else {
					for _, l := range lanNetworks() {
						if l.LocalIP == host {
							l := l
							here = &l
						}
					}
				}
			}
		}
		token := randomString(32)
		now := time.Now().Unix()
		name := cleanName(req.Name)
		a.remoteMu.Lock()
		if a.store.Home == nil && here != nil {
			a.store.Home = here
		}
		a.store.Devices = append(a.store.Devices, remoteDevice{
			ID: randomString(9), Name: name, IP: ip, ViaTailscale: viaTS, TSName: tsName,
			TokenHash: hashToken(token), Created: now, LastSeen: now,
		})
		err := a.saveRemoteStore()
		a.remoteMu.Unlock()
		if err != nil {
			http.Error(w, "couldn't save the pairing", http.StatusInternalServerError)
			return
		}
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "remote-paired")
			// Make a pairing visible, in case it wasn't you.
			where := "home Wi-Fi, " + ip
			if viaTS {
				where = "Tailscale, " + ip
				if tsName != "" {
					where = "Tailscale device “" + tsName + "”, " + ip
				}
			}
			runtime.EventsEmit(a.ctx, "notice", fmt.Sprintf(
				"A phone (%s, %s) just paired with the remote control. Not you? Remove it in Settings → Remote control.", name, where))
		}
		writeJSON(w, map[string]string{"token": token})
	})

	api := func(pattern string, h func(w http.ResponseWriter, r *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if _, ok := a.device(r); !ok {
				// A valid key is never rate-limited; wrong ones are (by
				// address), separately from wrong pairing codes, so a phone
				// with an old key can still pair again.
				ip, _ := privateClient(r.RemoteAddr)
				if rs.tooMany(rs.keyFails, limitKey(ip)) {
					http.Error(w, "too many failed attempts; try again in a few minutes", http.StatusTooManyRequests)
					return
				}
				rs.fail(rs.keyFails, limitKey(ip))
				http.Error(w, "not paired", http.StatusUnauthorized)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxTorrentUpload+1<<20)
			if err := h(w, r); err != nil {
				msg := phoneError(err)
				if msg != err.Error() {
					a.notifyError(err) // the details, with paths, on the computer only
				}
				http.Error(w, msg, http.StatusBadRequest)
			}
		})
	}
	api("GET /api/state", func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, a.phoneState())
		return nil
	})
	api("POST /api/magnet", func(w http.ResponseWriter, r *http.Request) error {
		var req struct{ Link string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return errors.New("bad request")
		}
		return a.AddMagnet(req.Link)
	})
	api("POST /api/torrent", func(w http.ResponseWriter, r *http.Request) error {
		f, _, err := r.FormFile("file")
		if err != nil {
			return errors.New("no file")
		}
		defer f.Close()
		tmp, err := os.CreateTemp("", "sharkbite-remote-*.torrent")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, err = io.Copy(tmp, io.LimitReader(f, maxTorrentUpload))
		tmp.Close()
		if err != nil {
			return err
		}
		return a.addSource(tmp.Name()) // keeps its own copy
	})
	api("POST /api/torrents/{hash}/{action}", func(w http.ResponseWriter, r *http.Request) error {
		hash := r.PathValue("hash")
		if !hashPattern.MatchString(hash) {
			return errors.New("bad torrent")
		}
		switch r.PathValue("action") {
		case "pause":
			return a.Pause(hash)
		case "resume":
			return a.resumeRemote(hash) // never accepts warnings: the phone doesn't show them
		case "remove":
			return a.Remove(hash, r.URL.Query().Get("files") == "1")
		}
		return errors.New("unknown action")
	})
	api("POST /api/all/{action}", func(w http.ResponseWriter, r *http.Request) error {
		switch r.PathValue("action") {
		case "pause":
			a.PauseAll()
		case "resume":
			a.ResumeAll()
		default:
			return errors.New("unknown action")
		}
		return nil
	})
	api("GET /api/search", func(w http.ResponseWriter, r *http.Request) error {
		res, err := a.SearchTorrents(r.URL.Query().Get("q"), r.URL.Query().Get("cat"))
		if err != nil {
			return err
		}
		writeJSON(w, rs.keepResults(res))
		return nil
	})
	api("POST /api/search/add", func(w http.ResponseWriter, r *http.Request) error {
		var req struct{ ID string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return errors.New("bad request")
		}
		res, ok := rs.result(req.ID)
		if !ok {
			return errors.New("that search result has expired; search again")
		}
		return a.AddSearchResult(res)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rs.allowedClient(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !hostIsAddress(r.Host) {
			http.Error(w, "use this computer's address, not a domain name", http.StatusMisdirectedRequest)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; manifest-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// --- bound to the frontend ---

func (a *App) GetRemoteStatus() RemoteStatus {
	a.mu.Lock()
	st := RemoteStatus{Enabled: a.settings.RemoteEnabled, Running: a.remote != nil, Error: a.remoteErr, Paused: a.remotePaused, Tailscale: a.settings.RemoteTailscale}
	var addrs []string
	tsAddr := ""
	if a.remote != nil {
		addrs, tsAddr = a.remote.addrs, a.remote.tsAddr
	}
	a.mu.Unlock()
	st.Port = a.remotePort()
	for _, ip := range addrs {
		u := "http://" + net.JoinHostPort(ip, strconv.Itoa(st.Port)) + "/"
		if ip == tsAddr {
			st.TailscaleURL = u
		} else {
			st.URLs = append(st.URLs, u)
		}
	}
	st.TailscaleFound = tsAddr != "" || tailscaleIP() != ""
	a.remoteMu.Lock()
	home := a.store.Home
	a.remoteMu.Unlock()
	for _, l := range lanNetworks() {
		if home == nil || l.same(*home) {
			st.LanAvailable = true
		}
	}
	a.remoteMu.Lock()
	if a.store.Home != nil {
		st.Home = a.store.Home.Gateway
	}
	if a.storeErr != nil && st.Error == "" {
		st.Error = a.storeErr.Error()
	}
	st.Devices = []RemoteDeviceView{}
	for _, d := range a.store.Devices {
		st.Devices = append(st.Devices, RemoteDeviceView{ID: d.ID, Name: d.Name, IP: d.IP, ViaTailscale: d.ViaTailscale, TSName: d.TSName, Created: d.Created, LastSeen: d.LastSeen})
	}
	a.remoteMu.Unlock()
	return st
}

// SetRemote turns the remote control on or off and sets its port.
func (a *App) SetRemote(enabled bool, port int) (RemoteStatus, error) {
	if port == 0 {
		port = defaultRemotePort
	}
	if port < 1024 || port > 65535 {
		return a.GetRemoteStatus(), errors.New("use a port between 1024 and 65535")
	}
	a.remoteOpMu.Lock()
	defer a.remoteOpMu.Unlock()
	a.stopRemote()
	a.mu.Lock()
	a.settings.RemoteEnabled, a.settings.RemotePort = enabled, port
	a.mu.Unlock()
	if err := a.writeSettings(); err != nil {
		return a.GetRemoteStatus(), err
	}
	a.startRemote()
	return a.GetRemoteStatus(), nil
}

// StartRemotePairing makes a one-time pairing code and its QR code.
// viaTailscale pairs on the Tailscale address (works at home and away; a
// phone keeps one key per address, so this is the one to use if it'll be
// used from outside).
func (a *App) StartRemotePairing(viaTailscale bool) (RemotePairing, error) {
	a.mu.Lock()
	enabled := a.settings.RemoteEnabled
	a.mu.Unlock()
	if !enabled {
		return RemotePairing{}, errors.New("turn the remote control on first")
	}
	if !viaTailscale {
		// Open the home-Wi-Fi listener for this pairing if there's no home
		// network yet (it's off until a phone pairs over home Wi-Fi).
		a.remoteMu.Lock()
		noHome := a.store.Home == nil
		a.remoteMu.Unlock()
		if noHome {
			a.mu.Lock()
			a.lanPairUntil = time.Now().Add(pairCodeLifetime)
			a.mu.Unlock()
			a.remoteOpMu.Lock()
			a.stopRemote()
			a.startRemote()
			a.remoteOpMu.Unlock()
		}
	}
	a.mu.Lock()
	rs := a.remote
	paused := a.remotePaused
	a.mu.Unlock()
	if rs == nil {
		if paused != "" {
			return RemotePairing{}, errors.New("the remote control is " + paused)
		}
		return RemotePairing{}, errors.New("turn the remote control on first")
	}
	code := newPairCode()
	until := time.Now().Add(pairCodeLifetime)
	rs.mu.Lock()
	rs.pairCode, rs.pairUntil, rs.pairFailures = code, until, 0
	rs.mu.Unlock()
	// The code is in the #fragment: the browser doesn't send that to any
	// server or put it in logs; the page reads it and posts it once.
	host := ""
	for _, ip := range rs.addrs {
		if (ip == rs.tsAddr) == viaTailscale {
			host = ip
			break
		}
	}
	if host == "" {
		if viaTailscale {
			return RemotePairing{}, errors.New("Tailscale isn't connected on this computer")
		}
		return RemotePairing{}, errors.New("this computer isn't on its home network right now; pair over Tailscale instead")
	}
	u := "http://" + net.JoinHostPort(host, strconv.Itoa(rs.port)) + "/#pair=" + code
	png, err := qrcode.Encode(u, qrcode.Medium, 320)
	if err != nil {
		return RemotePairing{}, err
	}
	return RemotePairing{URL: u, Code: code[:4] + "-" + code[4:], QR: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), Expires: until.Unix()}, nil
}

func (a *App) RemoveRemoteDevice(id string) error {
	a.remoteMu.Lock()
	defer a.remoteMu.Unlock()
	out := a.store.Devices[:0]
	for _, d := range a.store.Devices {
		if d.ID != id {
			out = append(out, d)
		}
	}
	a.store.Devices = out
	return a.saveRemoteStore()
}

// SetRemoteTailscale turns Tailscale access on or off.
func (a *App) SetRemoteTailscale(on bool) (RemoteStatus, error) {
	a.remoteOpMu.Lock()
	defer a.remoteOpMu.Unlock()
	a.stopRemote()
	a.mu.Lock()
	a.settings.RemoteTailscale = on
	a.mu.Unlock()
	if err := a.writeSettings(); err != nil {
		return a.GetRemoteStatus(), err
	}
	a.startRemote()
	return a.GetRemoteStatus(), nil
}

// ForgetRemoteHome clears the home network: the remote control then runs on
// whatever local network the computer is on, until a phone pairs there.
// For moving house or changing routers.
func (a *App) ForgetRemoteHome() (RemoteStatus, error) {
	a.remoteMu.Lock()
	a.store.Home = nil
	err := a.saveRemoteStore()
	a.remoteMu.Unlock()
	if err != nil {
		return a.GetRemoteStatus(), err
	}
	a.remoteOpMu.Lock()
	a.stopRemote()
	a.startRemote()
	a.remoteOpMu.Unlock()
	return a.GetRemoteStatus(), nil
}

// pathLike matches file paths (/home/…, C:\Users\…) in error messages.
var pathLike = regexp.MustCompile(`(?:[A-Za-z]:\\|/)[^\s:"']+`)

// phoneError is what the phone is told about err: its message, or a generic
// one if it mentions a file path, since the phone never gets this computer's
// folders or user name (see PRIVACY.md), and paths can contain spaces, which
// no pattern can reliably cut out.
func phoneError(err error) string {
	if pathLike.MatchString(err.Error()) {
		return "That didn't work on the computer; the details are shown there."
	}
	return err.Error()
}

// perDeviceListener refuses a device's connections beyond
// maxConnsPerDevice, so one device on the network can't hold every slot.
type perDeviceListener struct {
	net.Listener
	mu   sync.Mutex
	open map[string]int
}

func (l *perDeviceListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		l.mu.Lock()
		full := l.open[host] >= maxConnsPerDevice
		if !full {
			l.open[host]++
		}
		l.mu.Unlock()
		if full {
			c.Close()
			continue
		}
		return &deviceConn{Conn: c, l: l, host: host}, nil
	}
}

type deviceConn struct {
	net.Conn
	l    *perDeviceListener
	host string
	once sync.Once
}

func (c *deviceConn) Close() error {
	c.once.Do(func() {
		c.l.mu.Lock()
		if c.l.open[c.host]--; c.l.open[c.host] <= 0 {
			delete(c.l.open, c.host)
		}
		c.l.mu.Unlock()
	})
	return c.Conn.Close()
}
