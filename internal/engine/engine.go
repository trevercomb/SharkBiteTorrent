// Package engine wraps anacrolix/torrent so that every network operation it
// performs (peers, trackers, DHT, DNS) goes through the tunnel.
// No OS sockets are opened by the torrent client itself.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/dialer"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"

	"sharkbite/internal/dnsroute"
	"sharkbite/internal/scan"
	"sharkbite/internal/tunnel"
)

var defaultBootstrap = []string{
	"router.bittorrent.com:6881",
	"router.utorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"dht.libtorrent.org:25401",
}

type Options struct {
	// ForwardedPort, if non-zero, is a port the VPN provider forwards to us.
	// The engine accepts inbound peers on it and announces it to trackers/DHT.
	ForwardedPort int
	// Speed limits in bytes/second; 0 means unlimited.
	DownLimit int64
	UpLimit   int64
	// StateDir holds the piece-completion database (the app data folder).
	// Empty keeps completion in memory only (tests and tools).
	StateDir string
}

type Engine struct {
	cl       *torrent.Client
	dht      *dht.Server
	listener net.Listener
	tun      *tunnel.Tunnel
	down, up *rate.Limiter
	pc       storage.PieceCompletion // shared by all torrents; persists progress
	ownedDir string                  // fileStore ownership records ("" without StateDir)

	mu      sync.Mutex
	entries map[metainfo.Hash]*entry
}

// entry is our per-torrent state on top of anacrolix's.
type entry struct {
	t         *torrent.Torrent
	dir       string
	storage   *fileStore
	paused    bool
	skipped   map[int]bool // file indexes the user doesn't want
	high      map[int]bool // file indexes the user wants first (priority High)
	// firstLast: each wanted file's first and last piece come first, so a
	// video can be previewed (players need its start and its index at the end).
	firstLast bool
	added     time.Time
	warnings  []scan.Warning // from the file list, computed once metadata arrives
	inspected bool
	checking  atomic.Bool // verifying existing data after a restart
	// findingPeers: a DHT lookup for it is running (see findPeers).
	findingPeers atomic.Bool
	removing     bool  // Remove is deleting its files; guarded by Engine.mu
	stopped      bool  // blocked, and data transfer turned off; guarded by Engine.mu
	writeErr     error // writing its data failed (not its drive going away); paused until resumed; guarded by Engine.mu
	held         bool  // paused because of its warnings, until accepted and resumed; guarded by Engine.mu
	accepted     bool  // the user accepted its warnings (see AcceptWarnings); guarded by Engine.mu
	waiting      bool  // download folder unavailable: data transfer off until it's back; guarded by Engine.mu

	// "Verify files": re-hash finished pieces on disk (see Verify).
	verifying            atomic.Bool
	verifyDone, verifyOf atomic.Int64
	verified             *VerifyResult // last result; guarded by Engine.mu

	last               time.Time
	lastRead, lastWrit int64
	downRate, upRate   int64
}

// New starts a torrent client bound entirely to tun. With a nil tun (the
// user chose "No VPN") it uses the computer's normal network instead; see
// newDirect. The VPN path below never falls back to OS sockets.
func New(tun *tunnel.Tunnel, opts Options) (*Engine, error) {
	if tun == nil {
		return newDirect(opts)
	}
	tnet := tun.Net()

	// Force every default DNS lookup in this process through the tunnel,
	// regardless of which nameserver the OS would use. Covers UDP tracker
	// hostnames, which anacrolix resolves with net.ResolveUDPAddr.
	dnsroute.UseTunnel(func(ctx context.Context, network string) (net.Conn, error) {
		return tun.DialContext(ctx, network, tun.DNSServer())
	})

	e := &Engine{
		tun:     tun,
		down:    rate.NewLimiter(unlimited, 0),
		up:      rate.NewLimiter(unlimited, 0),
		entries: map[metainfo.Hash]*entry{},
	}
	e.SetLimits(opts.DownLimit, opts.UpLimit)
	if opts.StateDir != "" {
		e.ownedDir = filepath.Join(opts.StateDir, "owned")
		pc, err := storage.NewBoltPieceCompletion(opts.StateDir)
		if err != nil {
			return nil, fmt.Errorf("opening download progress database: %w", err)
		}
		e.pc = pc
	} else {
		e.pc = storage.NewMapPieceCompletion()
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.Slogger = redactingLogger() // no tracker passkeys in logs
	cfg.Seed = true
	// No web seeds (HTTP mirrors listed in a torrent). anacrolix/torrent
	// v1.61.0 crashes the whole app when a torrent with active web seed
	// requests is removed: the requests outlive the torrent and a timer's
	// consistency check panics (see the comment in its Torrent.close).
	// Peers still serve everything; mirrors often refuse ranged requests.
	cfg.DisableWebseeds = true

	// Connect to more peers, faster. The library's defaults assume OS sockets
	// (where Windows limits half-open connections) and wait 20s per dial. Our
	// dials go through the userspace tunnel, which has no such limits, and
	// behind a VPN without inbound connections many swarm members are
	// firewalled and never answer, so short timeouts and more parallel
	// attempts reach the connectable peers much sooner.
	cfg.NominalDialTimeout = 8 * time.Second
	// Effectively unlimited for real swarms (most peers behind a VPN are
	// unreachable anyway); still bounded so one huge swarm can't exhaust memory.
	// Bandwidth is controlled with the speed limits, not the peer count.
	cfg.EstablishedConnsPerTorrent = 200
	cfg.HalfOpenConnsPerTorrent = 60
	cfg.TotalHalfOpenConns = 250
	cfg.DialRateLimiter = rate.NewLimiter(40, 40)
	// Check (hash) and write finished pieces in parallel. Each check now also
	// writes the piece to disk in one go, which on a NAS means network
	// latency; with the default 2 hashers, checking fell far behind the
	// download and unchecked pieces piled up in memory.
	cfg.PieceHashersPerTorrent = 16
	// Torrents get their own storage (see Add); this only catches anything
	// added without one, and is never written to in practice.
	cfg.DataDir = os.TempDir()
	cfg.DownloadRateLimiter = e.down
	cfg.UploadRateLimiter = e.up

	// No OS sockets: we add tunnel-backed dialers/listeners/DHT below instead.
	cfg.DisableTCP = true
	cfg.DisableUTP = true
	cfg.NoDHT = true
	cfg.NoDefaultPortForwarding = true // UPnP would map a port on the real LAN
	cfg.DisableWebtorrent = true       // WebRTC uses OS sockets
	cfg.AcceptPeerConnections = opts.ForwardedPort > 0

	cfg.TrackerDialContext = verifiedTrackerDial(tun.DialContext) // HTTPS trackers get real certificate checks
	cfg.HTTPDialContext = tun.DialContext
	cfg.TrackerListenPacket = func(network, addr string) (net.PacketConn, error) {
		return tun.ListenUDP(network)
	}
	cfg.DhtStartingNodes = func(string) dht.StartingNodesGetter {
		return func() ([]dht.Addr, error) { return resolveBootstrap(tnet.LookupHost) }
	}

	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("torrent client: %w", err)
	}
	e.cl = cl
	cl.AddDialer(dialer.WithNetwork{Network: "tcp", Dialer: tun})

	if opts.ForwardedPort > 0 {
		l, err := tun.ListenTCP4(opts.ForwardedPort)
		if err != nil {
			cl.Close()
			return nil, fmt.Errorf("listen on forwarded port %d: %w", opts.ForwardedPort, err)
		}
		e.listener = l
		cl.AddListener(l) // also makes the client announce this port
	}

	pc, err := tun.ListenUDP4()
	if err != nil {
		e.closeClient()
		return nil, fmt.Errorf("dht socket: %w", err)
	}
	ds, err := cl.NewAnacrolixDhtServer(pc)
	if err != nil {
		pc.Close()
		e.closeClient()
		return nil, fmt.Errorf("dht server: %w", err)
	}
	e.dht = ds
	cl.AddDhtServer(torrent.AnacrolixDhtServerWrapper{Server: ds})
	go e.peerWatchdog()
	return e, nil
}

// peerWatchdog asks the DHT again, once a minute, for every torrent that
// still needs data (or its file list) but has no peers connected. The
// library's own announcer only runs when something wakes it, and a torrent
// could sit at 0 peers indefinitely: one restored at start from a magnet
// link with no trackers stayed at "Fetching metadata" until Pause/Resume.
// wantsData reports whether the torrent is missing data of a file the user
// wants (skipped files don't count: a torrent done with what's wanted isn't
// looking for peers).
func wantsData(t *torrent.Torrent) bool {
	for _, f := range t.Files() {
		if f.Priority() != torrent.PiecePriorityNone && f.BytesCompleted() < f.Length() {
			return true
		}
	}
	return false
}

func (e *Engine) peerWatchdog() {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-e.cl.Closed():
			return
		case <-tick.C:
		}
		e.mu.Lock()
		var idle []*entry
		for _, en := range e.entries {
			if en.t != nil && !en.paused && !en.waiting && !en.removing && !en.stopped && !en.held {
				idle = append(idle, en)
			}
		}
		e.mu.Unlock()
		for _, en := range idle {
			t := en.t
			if t.Stats().ActivePeers > 0 || t.Info() != nil && !wantsData(t) {
				continue
			}
			e.findPeers(en)
		}
	}
}

// newDirect starts the client on the computer's own network, for users who
// chose "No VPN" (for example because their router or system already runs
// one). It uses the library's normal sockets: TCP, DHT and UDP trackers, plus
// UPnP/NAT-PMP so peers can connect in.
func newDirect(opts Options) (*Engine, error) {
	dnsroute.UseSystem()

	e := &Engine{
		down:    rate.NewLimiter(unlimited, 0),
		up:      rate.NewLimiter(unlimited, 0),
		entries: map[metainfo.Hash]*entry{},
	}
	e.SetLimits(opts.DownLimit, opts.UpLimit)
	if opts.StateDir != "" {
		e.ownedDir = filepath.Join(opts.StateDir, "owned")
		pc, err := storage.NewBoltPieceCompletion(opts.StateDir)
		if err != nil {
			return nil, fmt.Errorf("opening download progress database: %w", err)
		}
		e.pc = pc
	} else {
		e.pc = storage.NewMapPieceCompletion()
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.Slogger = redactingLogger() // no tracker passkeys in logs
	cfg.Seed = true
	cfg.DisableWebseeds = true // see New
	cfg.EstablishedConnsPerTorrent = 200
	cfg.PieceHashersPerTorrent = 16 // see New
	cfg.DataDir = os.TempDir()
	cfg.DownloadRateLimiter = e.down
	cfg.UploadRateLimiter = e.up
	cfg.DisableUTP = true // as in VPN mode: no uTP (see README)
	cfg.DisableWebtorrent = true
	// HTTPS trackers' certificates are checked here too (see trackertls.go).
	cfg.TrackerDialContext = verifiedTrackerDial((&net.Dialer{Timeout: 30 * time.Second}).DialContext)
	cfg.ListenPort = 0 // a free port; UPnP maps whichever we get

	cl, err := torrent.NewClient(cfg)
	if err != nil {
		e.pc.Close()
		return nil, fmt.Errorf("torrent client: %w", err)
	}
	e.cl = cl
	go e.peerWatchdog() // as in VPN mode (findPeers uses the library's DHT here)
	return e, nil
}

func resolveBootstrap(lookup func(string) ([]string, error)) ([]dht.Addr, error) {
	var addrs []dht.Addr
	for _, hp := range defaultBootstrap {
		host, port, _ := net.SplitHostPort(hp)
		ips, err := lookup(host)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			ua, err := net.ResolveUDPAddr("udp", net.JoinHostPort(ip, port))
			if err == nil {
				addrs = append(addrs, dht.NewAddr(ua))
			}
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no DHT bootstrap nodes resolved")
	}
	return addrs, nil
}

func (e *Engine) closeClient() {
	e.cl.Close()
	if e.listener != nil {
		e.listener.Close()
	}
	// After the client: dropping torrents flushes their last completion writes.
	e.pc.Close()
}

// Close stops the client. Call before closing the tunnel.
func (e *Engine) Close() {
	e.closeClient()
	// Servers added via AddDhtServer aren't closed by the client, and the DHT
	// read loop panics if its socket dies while it's still open.
	if e.dht != nil {
		e.dht.Close()
	}
	// The tunnel is going away. Lookups still in flight in the library
	// (tracker announces outlive Close) mustn't fall back to the system's
	// DNS: nowhere until the next engine starts.
	if e.tun != nil {
		dnsroute.UseNone()
	}
}

// SetLimits changes the global speed limits (bytes/s, 0 = unlimited) live.
func (e *Engine) SetLimits(down, up int64) {
	setLimit(e.down, down)
	setLimit(e.up, up)
}

// minBurst: the library reads up to 1 MiB at a time through the download
// limiter (rate.go), so its burst can't be smaller.
const minBurst = 1 << 20

// setLimit sets a limiter's rate and its burst together: one second's worth,
// at least minBurst. The burst matters as much as the rate: a limiter made
// "unlimited" got a burst of 1 TiB from the library (it sizes a zero burst
// from the rate), so a limit set later, while the app ran, never held
// (seen: 12.5 MB/s at a 300 KB/s limit); only one set before starting did.
func setLimit(l *rate.Limiter, n int64) {
	burst := minBurst
	if n > minBurst {
		burst = int(min(n, 1<<30))
	}
	l.SetLimit(limitOf(n))
	l.SetBurst(burst)
}

// unlimited is used instead of rate.Inf: anacrolix stops opening peer
// connections when the download limiter reports no tokens, and an Inf limiter
// never refills its token count from zero. A huge finite rate refills instantly.
const unlimited = rate.Limit(1 << 40)

func limitOf(n int64) rate.Limit {
	if n <= 0 {
		return unlimited
	}
	return rate.Limit(n)
}

type AddOptions struct {
	Source string // magnet link or path to a .torrent file
	Dir    string // folder to save data in
	// Adopt: the torrent was saved by an older version, which wrote no claim
	// records: its existing files are adopted as its own (see ownership.go).
	Adopt   bool
	Skipped []int // file indexes not to download
	High    []int // file indexes to download first
	// FirstLast: each wanted file's first and last piece first (see entry).
	FirstLast bool
	Paused    bool
	// Recheck verifies data already on disk before downloading. Set it when
	// restoring a torrent after a restart: the storage forgets the progress of
	// unfinished (.part) files, and verifying recovers it.
	Recheck bool
	// AcceptWarnings: the user has seen this torrent's warnings (a disguised
	// program, see scan.Inspect) and resumed it anyway. Otherwise a torrent
	// with warnings is paused as soon as its file list arrives.
	AcceptWarnings bool
}

// heldMessage is shown for a torrent paused because of its warnings.
const heldMessage = "it was paused because it looks like a disguised program (see below). Press Resume only if you trust it."

// ErrWarnings: resuming a torrent whose warnings the user hasn't accepted
// (from the phone, or Resume all), which only the computer's window shows.
var ErrWarnings = errors.New("this torrent looks like a disguised program; check its warning in the app on the computer, and resume it there if you trust it")

// Add starts a torrent and returns its info hash.
func (e *Engine) Add(o AddOptions) (string, error) {
	var spec *torrent.TorrentSpec
	var err error
	if strings.HasPrefix(o.Source, "magnet:") {
		spec, err = torrent.TorrentSpecFromMagnetUri(o.Source)
	} else {
		var mi *metainfo.MetaInfo
		if mi, err = metainfo.LoadFromFile(o.Source); err == nil {
			spec, err = torrent.TorrentSpecFromMetaInfoErr(mi)
		}
	}
	if err != nil {
		return "", err
	}

	e.mu.Lock()
	if old, ok := e.entries[spec.InfoHash]; ok {
		e.mu.Unlock()
		if old.removing {
			// Its files are still being deleted: adding it now would
			// create files the old removal then deletes.
			return "", errors.New("this torrent is still being removed; try again in a moment")
		}
		return spec.InfoHash.HexString(), nil
	}
	st := newFileStore(o.Dir, e.pc, e.ownedDir, o.Adopt)
	spec.Storage = st
	en := &entry{dir: o.Dir, storage: st, paused: o.Paused, accepted: o.AcceptWarnings, skipped: map[int]bool{}, high: map[int]bool{},
		firstLast: o.FirstLast, added: time.Now()}
	for _, i := range o.Skipped {
		en.skipped[i] = true
	}
	for _, i := range o.High {
		en.high[i] = true
	}
	e.entries[spec.InfoHash] = en
	e.mu.Unlock()

	// A .torrent's file list is set only after downloading is turned off
	// (adding it with the list lets a web seed start at once).
	infoBytes := spec.InfoBytes
	spec.InfoBytes = nil
	t, _, err := e.cl.AddTorrentSpec(spec)
	if err == nil {
		en.t = t
		// No data until its file list has been checked (below), so a
		// torrent that looks like a disguised program never starts
		// downloading.
		t.DisallowDataDownload()
		if infoBytes != nil {
			if err = t.SetInfoBytes(infoBytes); err != nil {
				t.Drop()
			}
		}
	}
	if err != nil {
		e.mu.Lock()
		delete(e.entries, spec.InfoHash)
		e.mu.Unlock()
		return "", err
	}
	// A finished piece that couldn't be saved still counts as finished for
	// the library (it ignores MarkComplete's error): re-checked from the
	// drive, so it's dropped and downloaded again if it isn't there.
	st.mu.Lock()
	st.onMarkFailed = func(idx int) { go t.Piece(idx).VerifyData() }
	st.mu.Unlock()
	// Writing failed: the library would quietly stop downloading. Its drive
	// going away waits for it; anything else (a full disk, a file that
	// isn't ours) pauses it with the reason.
	t.SetOnWriteChunkError(func(werr error) {
		if st.checkGone() {
			e.waitForFolder(en, false)
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if en.writeErr == nil {
			en.writeErr, en.paused = werr, true
			t.DisallowDataDownload()
			t.DisallowDataUpload()
		}
	})
	if o.Paused {
		t.DisallowDataUpload()
	}
	go func() {
		select {
		case <-t.GotInfo():
			w := inspect(t)
			e.mu.Lock()
			en.warnings, en.inspected = w, true
			// Held even if already paused (so Resume all and the phone leave
			// it), until the user accepts its warnings on the computer.
			en.held = len(w) > 0 && !en.accepted
			if en.held {
				en.paused = true
			}
			blocked := st.Blocked() != nil
			switch {
			case blocked || en.paused:
				// Blocked: its files aren't safe to write (shown in the
				// list). Download too: the folder check may have turned it
				// on meanwhile.
				t.DisallowDataDownload()
				t.DisallowDataUpload()
			default:
				t.AllowDataDownload()
			}
			e.mu.Unlock()
			if blocked {
				return
			}
			e.applyPriorities(en)
			if st.Unavailable() != nil {
				e.waitForFolder(en, o.Recheck)
				return
			}
			if o.Recheck {
				recheck(en)
			}
		case <-t.Closed():
		}
	}()
	return t.InfoHash().HexString(), nil
}

// applyPriorities gives each file its priority (skipped: None, high: High,
// else Normal) and, with firstLast, raises each wanted file's first and last
// piece to Next: above any file's priority (High included, or a High file's
// ends would be no different from its middle), below Now (live reading).
func (e *Engine) applyPriorities(en *entry) {
	e.mu.Lock()
	skipped, high := maps.Clone(en.skipped), maps.Clone(en.high)
	firstLast := en.firstLast
	e.mu.Unlock()
	files := en.t.Files()
	for i, f := range files {
		switch {
		case skipped[i]:
			f.SetPriority(torrent.PiecePriorityNone)
		case high[i]:
			f.SetPriority(torrent.PiecePriorityHigh)
		default:
			f.SetPriority(torrent.PiecePriorityNormal)
		}
	}
	// The ends of skipped files first, so a piece a wanted file shares with
	// a skipped neighbour keeps the wanted file's setting.
	for _, wanted := range []bool{false, true} {
		for i, f := range files {
			if skipped[i] == wanted || f.Length() == 0 {
				continue
			}
			prio := torrent.PiecePriorityNone // no piece-level boost: the file's own priority applies
			if firstLast && wanted {
				prio = torrent.PiecePriorityNext
			}
			en.t.Piece(f.BeginPieceIndex()).SetPriority(prio)
			en.t.Piece(f.EndPieceIndex() - 1).SetPriority(prio)
		}
	}
}

func (e *Engine) find(hash string) (*entry, error) {
	var h metainfo.Hash
	if err := h.FromHexString(hash); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	en, ok := e.entries[h]
	if !ok || en.t == nil || en.removing {
		return nil, fmt.Errorf("torrent %s: %w", hash, ErrNotFound)
	}
	return en, nil
}

func (e *Engine) Pause(hash string) error {
	en, err := e.find(hash)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	en.paused = true
	en.t.DisallowDataDownload()
	en.t.DisallowDataUpload()
	return nil
}

func (e *Engine) Resume(hash string) error {
	en, err := e.find(hash)
	if err != nil {
		return err
	}
	if err := en.storage.Blocked(); err != nil {
		return err
	}
	if err := en.storage.Unavailable(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(en.warnings) > 0 && !en.accepted {
		return ErrWarnings
	}
	e.resumeLocked(en)
	return nil
}

// ResumeAccepting is Resume from the app's window, where the torrent's
// warnings are shown: shown is how many it displayed. It accepts them
// (reported by accepted) only together with resuming, and only if they're
// the ones shown: a file list checked since then could have added some.
func (e *Engine) ResumeAccepting(hash string, shown int) (accepted bool, err error) {
	en, err := e.find(hash)
	if err != nil {
		return false, err
	}
	if err := en.storage.Blocked(); err != nil {
		return false, err
	}
	if err := en.storage.Unavailable(); err != nil {
		return false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(en.warnings) > 0 && !en.accepted {
		if len(en.warnings) != shown {
			return false, ErrWarnings
		}
		en.accepted, accepted = true, true
	}
	e.resumeLocked(en)
	return accepted, nil
}

func (e *Engine) resumeLocked(en *entry) {
	en.paused, en.held, en.writeErr = false, false, nil
	if en.inspected { // before that, the check turns downloading on (see Add)
		en.t.AllowDataDownload()
	}
	en.t.AllowDataUpload()
	e.findPeers(en)
}

// findPeers asks the DHT for the torrent's peers now. The library looks for
// peers only when something wakes its announcer, and a torrent that was
// finished (connected to no one: seeders drop each other) and needs pieces
// again, after Verify found damaged ones or its drive came back, was never
// woken: it sat at 0 peers until the app restarted.
func (e *Engine) findPeers(en *entry) {
	// The tunnel's DHT; with No VPN, the library's own.
	var ds torrent.DhtServer
	if e.dht != nil {
		ds = torrent.AnacrolixDhtServerWrapper{Server: e.dht}
	} else if servers := e.cl.DhtServers(); len(servers) > 0 {
		ds = servers[0]
	}
	if ds == nil || en.t == nil || !en.findingPeers.CompareAndSwap(false, true) {
		return // one lookup at a time per torrent
	}
	t := en.t
	go func() {
		defer en.findingPeers.Store(false)
		done, stop, err := t.AnnounceToDht(ds)
		if err != nil {
			return
		}
		defer stop()
		select {
		case <-done:
		case <-t.Closed():
		case <-time.After(2 * time.Minute):
		}
	}()
}

// HasWarnings reports whether the torrent's file list got warnings (see
// scan.Inspect) that the user hasn't accepted.
func (e *Engine) HasWarnings(hash string) bool {
	en, err := e.find(hash)
	if err != nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(en.warnings) > 0 && !en.accepted
}

// inspect checks the torrent's file list for disguised programs.
func inspect(t *torrent.Torrent) []scan.Warning {
	top := ""
	if info := t.Info(); info != nil && info.IsDir() {
		top = info.BestName() // multi-file paths don't include it
	}
	var fs []scan.File
	for _, f := range t.Files() {
		fs = append(fs, scan.File{Path: f.DisplayPath(), Size: f.Length(), Top: top})
	}
	return scan.Inspect(fs)
}

// waitForFolder keeps the torrent's data transfer off while its download
// folder is unavailable (its drive disconnected), checking every 10 s, then
// claims its files (see fileStore.reclaim) and resumes, unless it's paused.
// Downloading meanwhile would only throw the data away. It returns at once
// if another call is already waiting for this torrent.
func (e *Engine) waitForFolder(en *entry, recheckAfter bool) {
	e.mu.Lock()
	if en.waiting {
		e.mu.Unlock()
		return
	}
	en.waiting = true
	e.mu.Unlock()
	en.t.DisallowDataDownload()
	en.t.DisallowDataUpload()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for en.storage.unavailableNow() != nil {
		select {
		case <-en.t.Closed():
			return
		case <-tick.C:
		}
	}
	err := en.storage.reclaim()
	e.mu.Lock()
	en.waiting = false
	// Turned on under the lock, so a Pause or a hold meanwhile wins; and
	// downloading only once the file list has been checked (see Add).
	resume := err == nil && !en.paused && !en.removing
	if resume {
		if en.inspected {
			en.t.AllowDataDownload()
		}
		en.t.AllowDataUpload()
		e.findPeers(en)
	}
	e.mu.Unlock()
	if resume && recheckAfter {
		recheck(en)
	}
}

// ErrNotFound: no such torrent (for Remove, already removed).
var ErrNotFound = errors.New("not found")

// ErrFolderUnavailable: Remove with files refused because the torrent's
// download folder (its drive) isn't there. Nothing was changed.
var ErrFolderUnavailable = errors.New("its download folder isn't available (is its drive connected?). Connect the drive and try again, or remove it without deleting its files")

// Remove drops the torrent. If deleteFiles is set, its downloaded files are
// deleted too: only the files it created (see ownership.go), and only once
// nothing has them open, then any of its folders left empty. Never a whole
// folder, and never a file it didn't create. The torrent stays reserved
// until then, so it can't be added again while its files are going.
func (e *Engine) Remove(hash string, deleteFiles bool) error {
	var h metainfo.Hash
	if err := h.FromHexString(hash); err != nil {
		return err
	}
	// Refused before anything changes: dropping it first would erase its
	// progress and take it off the list while its files stay on the drive.
	// Checked before taking the lock: on a hung network drive it can take a
	// while, and the list would freeze meanwhile.
	if deleteFiles {
		if en, err := e.find(hash); err == nil && (en.storage.Unavailable() != nil || en.storage.unavailableNow() != nil) {
			return ErrFolderUnavailable
		}
	}
	// Found and reserved in one step, so two Removes can't both proceed.
	e.mu.Lock()
	en, ok := e.entries[h]
	if !ok || en.t == nil || en.removing {
		e.mu.Unlock()
		return fmt.Errorf("torrent %s: %w", hash, ErrNotFound)
	}
	en.removing = true
	e.mu.Unlock()
	ih := en.t.InfoHash()
	en.storage.dropping() // no point writing out what's about to be deleted
	defer func() {
		e.mu.Lock()
		if e.entries[ih] == en {
			delete(e.entries, ih)
		}
		e.mu.Unlock()
	}()
	info := en.t.Info()
	en.t.Drop() // closes the storage
	stillOpen := en.storage.waitReleased(15 * time.Second)
	if info != nil {
		// Forget its progress, so re-adding it later starts clean; after
		// the wait, so a piece finishing meanwhile can't mark itself again.
		clearComplete(e.pc, ih, info.NumPieces())
	}
	own := en.storage.ownership()
	if own == nil && e.ownedDir != "" {
		// Never opened this session (a magnet still fetching its file
		// list): its record on disk says which files it created.
		own = loadOwnership(e.ownedDir, ih, en.dir, true)
	}
	if !deleteFiles {
		// The files stay, but they're no longer the torrent's: adding it
		// again is checked like any new torrent (and refused over them).
		if own != nil {
			own.forget()
		}
		return nil
	}
	if en.storage.Unavailable() != nil || en.storage.unavailableNow() != nil {
		// Its record stays, so adding it again once the drive is back
		// finds its files as its own.
		return errors.New("its download folder isn't available (is its drive connected?), so its files weren't deleted")
	}
	if own != nil && !own.written {
		own = nil // no record on disk (or only a v0.2.37 marker): nothing listed
	}
	var owned, prune []string
	switch {
	case own != nil:
		owned, prune = own.files(), own.folders()
	case info == nil:
		return errors.New("its file list isn't known yet (still fetching it), so its files weren't deleted")
	default:
		owned = en.storage.files() // no records (tests and tools)
	}
	var shared map[string]bool
	if e.ownedDir != "" {
		// Two torrents sharing a file (v0.2.36 allowed it): it stays for
		// the other one.
		shared = ownedByOthers(e.ownedDir, ih, owned)
	}
	var paths []string
	for _, p := range owned {
		if !stillOpen[p] && !shared[p] {
			paths = append(paths, p)
		}
	}
	err := removeFiles(en.dir, paths, prune, own == nil)
	if len(stillOpen) > 0 {
		return errors.Join(err, fmt.Errorf("%d file(s) were still in use, so they weren't deleted", len(stillOpen)))
	}
	if err == nil && own != nil {
		own.forget()
	}
	return err
}

// Claimed reports that the torrent passed its check (see ownership.go) and
// can write its files.
func (e *Engine) Claimed(hash string) bool {
	en, err := e.find(hash)
	if err != nil {
		return false
	}
	en.storage.mu.Lock()
	defer en.storage.mu.Unlock()
	return en.storage.ft != nil && en.storage.blocked == nil
}

// OwnedFiles are the torrent's files at its own paths (for marking them as
// downloaded: never other files in its folder).
func (e *Engine) OwnedFiles(hash string) []string {
	en, err := e.find(hash)
	if err != nil {
		return nil
	}
	return en.storage.files()
}

// removeFiles deletes paths (files inside dir), then the folders in prune
// that are left empty, deepest first. With parents, also every folder
// between the files and dir (no ownership records). Never dir itself or
// anything outside it.
func removeFiles(dir string, paths, prune []string, parents bool) error {
	var errs []error
	dirs := map[string]bool{}
	for _, d := range prune {
		dirs[d] = true
	}
	for _, p := range paths {
		// Windows can hold file handles briefly after Drop; retry a little.
		var err error
		for range 10 {
			if err = os.Remove(p); err == nil || errors.Is(err, os.ErrNotExist) {
				err = nil
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err != nil {
			errs = append(errs, err)
		}
		for d := filepath.Dir(p); parents && d != filepath.Clean(dir) && isSub(dir, d); d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	// Deepest first; os.Remove only removes empty folders.
	sorted := make([]string, 0, len(dirs))
	for d := range dirs {
		if d != filepath.Clean(dir) && isSub(dir, d) {
			sorted = append(sorted, d)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, d := range sorted {
		// Only a real, empty folder: os.Remove on a symlink or junction the
		// user made (named like the torrent) would remove the link itself.
		if fi, err := os.Lstat(d); err == nil && fi.IsDir() {
			os.Remove(d)
		}
	}
	return errors.Join(errs...)
}

type FileStatus struct {
	Index  int    `json:"index"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Done   int64  `json:"done"`
	Wanted bool   `json:"wanted"`
	High   bool   `json:"high"` // downloaded first (priority High)
}

// Files lists a torrent's files; empty until metadata is available.
func (e *Engine) Files(hash string) ([]FileStatus, error) {
	en, err := e.find(hash)
	if err != nil {
		return nil, err
	}
	if en.t.Info() == nil {
		return []FileStatus{}, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	files := en.t.Files()
	out := make([]FileStatus, len(files))
	for i, f := range files {
		out[i] = FileStatus{
			Index:  i,
			Path:   scan.StripBidiControls(f.DisplayPath()), // shown as-is: no direction tricks
			Size:   f.Length(),
			Done:   f.BytesCompleted(),
			Wanted: !en.skipped[i],
			High:   !en.skipped[i] && en.high[i],
		}
	}
	return out, nil
}

// SetFilesWanted changes which files are downloaded and returns the full
// list of skipped indexes (for persisting).
func (e *Engine) SetFilesWanted(hash string, indexes []int, wanted bool) ([]int, error) {
	en, err := e.find(hash)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	for _, i := range indexes {
		if wanted {
			delete(en.skipped, i)
		} else {
			en.skipped[i] = true
		}
	}
	skipped := make([]int, 0, len(en.skipped))
	for i := range en.skipped {
		skipped = append(skipped, i)
	}
	e.mu.Unlock()
	sort.Ints(skipped)
	if en.t.Info() != nil {
		e.applyPriorities(en)
	}
	return skipped, nil
}

// SetFilesHigh marks files to download before the others (or not) and
// returns the full list of such indexes (for persisting).
func (e *Engine) SetFilesHigh(hash string, indexes []int, high bool) ([]int, error) {
	en, err := e.find(hash)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	for _, i := range indexes {
		if high {
			en.high[i] = true
		} else {
			delete(en.high, i)
		}
	}
	out := slices.Sorted(maps.Keys(en.high))
	e.mu.Unlock()
	if en.t.Info() != nil {
		e.applyPriorities(en)
	}
	if out == nil {
		out = []int{}
	}
	return out, nil
}

// SetFirstLast turns "first and last pieces first" on or off for a torrent.
func (e *Engine) SetFirstLast(hash string, on bool) error {
	en, err := e.find(hash)
	if err != nil {
		return err
	}
	e.mu.Lock()
	en.firstLast = on
	e.mu.Unlock()
	if en.t.Info() != nil {
		e.applyPriorities(en)
	}
	return nil
}

type Status struct {
	Hash     string  `json:"hash"`
	Name     string  `json:"name"`
	Dir      string  `json:"dir"`
	State    string  `json:"state"` // metadata, needs-torrent-file, checking, downloading, seeding, paused
	Size     int64   `json:"size"`  // bytes of wanted files
	Done     int64   `json:"done"`  // completed bytes of wanted files
	Progress float64 `json:"progress"`
	DownRate int64   `json:"downRate"`
	UpRate   int64   `json:"upRate"`
	Peers    int     `json:"peers"`
	Seeds    int     `json:"seeds"`
	// Warnings from the file list (see scan.Inspect), known before downloading.
	Warnings []scan.Warning `json:"warnings"`
	// Problem: why the torrent is stopped (state "blocked").
	Problem string `json:"problem"`
	// Held: it has warnings the user hasn't accepted (see AcceptWarnings).
	Held bool `json:"held"`
	// FirstLast: its files' first and last pieces come first (SetFirstLast).
	FirstLast bool `json:"firstLast"`
	// Confirm: added from outside the app, waiting for the user to start
	// it; filled in by the app.
	Confirm bool `json:"confirm"`
	// Antivirus scan of the finished files; filled in by the app.
	ScanState  string   `json:"scanState"` // "", scanning, clean, threat, error, toolarge
	ScanEngine string   `json:"scanEngine"`
	Threats    []string `json:"threats"`
	ScanError  string   `json:"scanError"`
	// toolarge: files the antivirus skipped for their size; CanScanFully if
	// App.ScanFully would check at least one of them.
	TooLarge     []string `json:"tooLarge"`
	CanScanFully bool     `json:"canScanFully"`
	ScanSeen     bool     `json:"scanSeen"` // the user has opened the files panel since
	// "Verify files" (see Engine.Verify): progress while running, then the
	// last result. Bad pieces are downloaded again automatically.
	Verifying      bool          `json:"verifying"`
	VerifyProgress float64       `json:"verifyProgress"`
	Verified       *VerifyResult `json:"verified"`
	// .sfv check of the finished files; filled in by the app.
	SFV *SFVStatus `json:"sfv"`
	// Suspect: why its data should be verified before the malware scan
	// (its drive went away, a read or write failed); "" if nothing did.
	Suspect string `json:"suspect"`
}

// VerifyResult is the outcome of re-checking a torrent's data on disk.
type VerifyResult struct {
	Pieces   int      `json:"pieces"`   // pieces that were complete and got re-checked
	Bad      int      `json:"bad"`      // of those, how many no longer matched
	BadFiles []string `json:"badFiles"` // files containing bad pieces
	At       int64    `json:"at"`       // unix seconds
	// Reason: why it was verified automatically ("" if the user asked).
	Reason string `json:"reason"`
	// Unreadable: pieces whose data couldn't be read (not damaged, not
	// verified either: checked again later).
	Unreadable int `json:"unreadable"`
}

// SFVStatus is the result of checking the torrent's own .sfv file(s).
type SFVStatus struct {
	Checked  int      `json:"checked"`
	Bad      []string `json:"bad"`
	Missing  []string `json:"missing"`
	Error    string   `json:"error"`
	Checking bool     `json:"checking"`
}

// List returns a snapshot of all torrents. Rates are computed from the
// change since the previous call.
func (e *Engine) List() []Status {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()

	out := []Status{}
	for h, en := range e.entries {
		t := en.t
		if t == nil || en.removing {
			continue
		}
		st := t.Stats()
		read := st.BytesReadUsefulData.Int64()
		written := st.BytesWrittenData.Int64()
		if dt := now.Sub(en.last).Seconds(); dt >= 0.5 {
			if !en.last.IsZero() {
				en.downRate = int64(math.Max(0, float64(read-en.lastRead)/dt))
				en.upRate = int64(math.Max(0, float64(written-en.lastWrit)/dt))
			}
			en.last, en.lastRead, en.lastWrit = now, read, written
		}

		row := Status{
			Hash:     h.HexString(),
			Name:     scan.StripBidiControls(t.Name()),
			Dir:      en.dir,
			DownRate: en.downRate,
			UpRate:   en.upRate,
			Peers:    st.ActivePeers,
			Seeds:    st.ConnectedSeeders,
		}
		row.Warnings = en.warnings
		row.Held = len(en.warnings) > 0 && !en.accepted
		row.FirstLast = en.firstLast
		row.Verified = en.verified
		row.Suspect = en.storage.Suspect()
		if row.Suspect == "" && en.writeErr != nil {
			row.Suspect = "writing its files failed"
		}
		if en.verifying.Load() {
			row.Verifying = true
			if of := en.verifyOf.Load(); of > 0 {
				row.VerifyProgress = float64(en.verifyDone.Load()) / float64(of)
			}
		}
		if t.Info() != nil {
			for i, f := range t.Files() {
				if en.skipped[i] {
					continue
				}
				row.Size += f.Length()
				row.Done += f.BytesCompleted()
			}
			if row.Size > 0 {
				row.Progress = float64(row.Done) / float64(row.Size)
			} else {
				row.Progress = 1
			}
		}
		blocked := en.storage.Blocked()
		gone := en.storage.Unavailable()
		if gone != nil && blocked == nil && !en.waiting && t.Info() != nil {
			go e.waitForFolder(en, false) // became unavailable while running
		}
		if blocked != nil && !en.stopped {
			// Blocked after it started (a file appeared that isn't ours).
			en.stopped = true
			t.DisallowDataDownload()
			t.DisallowDataUpload()
		}
		switch {
		case blocked != nil:
			row.State, row.Problem = "blocked", blocked.Error()
		case gone != nil:
			row.State, row.Problem = "blocked", gone.Error()
		case en.paused:
			row.State = "paused"
			switch {
			case en.held:
				row.Problem = heldMessage
			case en.writeErr != nil:
				row.Problem = "writing its files failed (" + en.writeErr.Error() + "). Press Resume to try again"
			}
		case en.checking.Load():
			row.State = "checking"
		case t.Info() == nil && st.ActivePeers > 0 && now.Sub(en.added) > metadataStallAfter:
			// Connected, but nobody will send the file list: almost always a
			// private torrent, whose peers disable metadata exchange (ut_metadata).
			row.State = "needs-torrent-file"
		case t.Info() == nil:
			row.State = "metadata"
		case row.Done >= row.Size && piecesBeingChecked(t):
			// Every byte is there, but some pieces are still queued for
			// their hash check (after a restart, many are): not finished
			// yet, so no malware scan or automatic Verify starts on files
			// that aren't confirmed.
			row.State = "checking"
		case row.Done >= row.Size:
			row.State = "seeding"
		default:
			row.State = "downloading"
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// WriteStatus dumps the client's internal state (trackers, DHT, peers) for debugging.
func (e *Engine) WriteStatus(w io.Writer) { e.cl.WriteStatus(w) }

// metadataStallAfter is how long a magnet may sit connected to peers without
// receiving metadata before the UI suggests using the .torrent file instead.
const metadataStallAfter = 45 * time.Second

// recheck verifies the pieces of every unfinished file, so progress saved in
// .part files survives a restart. The storage marks those pieces incomplete
// on open (anacrolix's part-file mode only trusts finished files), so without
// this they would be downloaded again. It runs before priorities are applied,
// so nothing is re-requested while checking. Finished files are skipped.
func recheck(en *entry) {
	en.storage.clearReadFailed()
	t := en.t
	var pieces []int
	for _, f := range t.Files() {
		if f.BytesCompleted() < f.Length() {
			for i := f.BeginPieceIndex(); i < f.EndPieceIndex(); i++ {
				pieces = append(pieces, i)
			}
		}
	}
	if len(pieces) == 0 {
		return
	}
	en.checking.Store(true)
	defer en.checking.Store(false)

	// VerifyData queues the piece on the library's hashers and waits; run a
	// bounded number at once so the queue stays full.
	sem := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for _, i := range pieces {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			t.Piece(i).VerifyData()
		}(i)
	}
	wg.Wait()
}

// Verify re-reads every finished piece from disk and checks it against the
// torrent's piece hashes (SHA-1, or SHA-256 for v2 torrents). That catches
// damage after the download: a bad copy, a failing disk or NAS. Pieces that
// no longer match are marked missing, so the torrent downloads them again.
// It blocks until done; progress shows in List.
func (e *Engine) Verify(ctx context.Context, hash string) (*VerifyResult, error) {
	en, err := e.find(hash)
	if err != nil {
		return nil, err
	}
	t := en.t
	if t.Info() == nil {
		return nil, errors.New("the torrent's file list isn't known yet")
	}
	if err := en.storage.Unavailable(); err != nil {
		return nil, err // every piece would read as damaged
	}
	// A file that can't be opened (a NAS refuses access for a while to a
	// file the app had open when it was killed) would make every piece in
	// it fail: try again later instead. A missing file is checked: its
	// pieces really are gone.
	if err := en.storage.Blocked(); err != nil {
		return nil, err // its paths are taken by files that aren't its own
	}
	if paths, err := torrentPaths(en.dir, t.Info()); err == nil {
		for _, p := range paths {
			// Only regular files: opening a FIFO someone left at the path
			// would wait forever.
			if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			f, err := os.Open(p)
			if err != nil {
				return nil, fmt.Errorf("its files can't be read right now (%w); try again in a minute", err)
			}
			f.Close()
		}
	}
	if !en.verifying.CompareAndSwap(false, true) {
		return nil, errors.New("already verifying")
	}
	defer en.verifying.Store(false)

	var pieces []int
	for i := 0; i < t.NumPieces(); i++ {
		if t.PieceState(i).Complete {
			pieces = append(pieces, i)
		}
	}
	en.verifyDone.Store(0)
	en.verifyOf.Store(int64(len(pieces)))
	// Failures are collected as they happen: by the time all pieces are
	// checked, a damaged one found early may already be downloaded again,
	// and counting incomplete pieces at the end then reported "all match".
	stopLog := en.storage.startFailLog()
	defer stopLog() // also on the early returns (a second call returns nothing)

	var bad []int
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, i := range pieces {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, ctx.Err()
		}
		wg.Add(1)
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			t.Piece(i).VerifyDataContext(ctx)
			en.verifyDone.Add(1)
		}(i)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// VerifyData can return before the library has recorded the result, so
	// counting right away sometimes found no damaged pieces ("all match")
	// while the library then marked some missing. Wait until none of them
	// is still being checked or marked, then count.
	settle := time.Now().Add(15 * time.Second)
	for time.Now().Before(settle) {
		busy := false
		for _, i := range pieces {
			if s := t.PieceState(i); s.Checking || s.Hashing || s.QueuedForHash || s.Marking {
				busy = true
				break
			}
		}
		if !busy {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	verifying := map[int]bool{}
	for _, i := range pieces {
		verifying[i] = true
	}
	failed, unreadable := stopLog()
	for _, i := range failed {
		if verifying[i] { // not a piece that just failed while downloading
			bad = append(bad, i)
		}
	}
	unread := 0
	for _, i := range unreadable {
		if verifying[i] {
			unread++
		}
	}
	e.mu.Lock()
	running := !en.paused && !en.held && !en.stopped && !en.waiting
	e.mu.Unlock()
	if (len(bad) > 0 || unread > 0) && running {
		e.findPeers(en) // it needs pieces again: find someone to get them from
	}

	res := &VerifyResult{Pieces: len(pieces), Bad: len(bad), Unreadable: unread, BadFiles: []string{}, At: time.Now().Unix()}
	for _, f := range t.Files() {
		for _, p := range bad {
			if p >= f.BeginPieceIndex() && p < f.EndPieceIndex() {
				res.BadFiles = append(res.BadFiles, scan.StripBidiControls(f.DisplayPath()))
				break
			}
		}
	}
	e.mu.Lock()
	en.verified = res
	e.mu.Unlock()
	return res, nil
}

// FilePaths returns where each finished, wanted file is on disk, keyed by
// its path inside the torrent (forward slashes).
func (e *Engine) FilePaths(hash string) (map[string]string, error) {
	en, err := e.find(hash)
	if err != nil {
		return nil, err
	}
	info := en.t.Info()
	if info == nil {
		return nil, errors.New("the torrent's file list isn't known yet")
	}
	if _, err := torrentPaths(en.dir, info); err != nil {
		return nil, err
	}
	root := filepath.Join(en.dir, info.BestName())
	out := map[string]string{}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, f := range en.t.Files() {
		if en.skipped[i] || f.BytesCompleted() < f.Length() {
			continue
		}
		rel := f.DisplayPath()
		if info.IsDir() {
			out[rel] = filepath.Join(root, filepath.FromSlash(rel))
		} else {
			out[rel] = root
		}
	}
	return out, nil
}

// ContentPath returns where a torrent's data lives: its folder for
// multi-file torrents, or its file for single-file ones. ok is false until
// the torrent's metadata is known.
func (e *Engine) ContentPath(hash string) (path string, isDir bool, ok bool) {
	en, err := e.find(hash)
	if err != nil {
		return "", false, false
	}
	info := en.t.Info()
	if info == nil {
		return en.dir, true, false
	}
	if _, err := torrentPaths(en.dir, info); err != nil {
		return "", false, false // an unsafe name could point at the download folder itself
	}
	return filepath.Join(en.dir, info.BestName()), info.IsDir(), true
}

// PieceRun is a run of consecutive pieces in the same state, for the
// piece map. State is one of: done, checking, partial (downloading),
// missing, skipped (in files the user unticked).
type PieceRun struct {
	State string `json:"s"`
	Count int    `json:"n"`
}

// PieceMap returns the torrent's pieces as runs of states, in order. Empty
// until metadata is known.
func (e *Engine) PieceMap(hash string) ([]PieceRun, error) {
	en, err := e.find(hash)
	if err != nil {
		return nil, err
	}
	if en.t.Info() == nil {
		return []PieceRun{}, nil
	}
	var out []PieceRun
	for _, r := range en.t.PieceStateRuns() {
		s := "missing"
		switch {
		case r.Complete:
			s = "done"
		case r.Hashing || r.QueuedForHash || r.Marking || r.Checking:
			s = "checking"
		case r.Partial:
			s = "partial"
		case r.Priority == torrent.PiecePriorityNone:
			s = "skipped"
		}
		// Merge with the previous run if only fields we don't show differ.
		if n := len(out); n > 0 && out[n-1].State == s {
			out[n-1].Count += r.Length
		} else {
			out = append(out, PieceRun{State: s, Count: r.Length})
		}
	}
	return out, nil
}

// VerifyAuto is Verify run by the app on its own (see App.autoVerify):
// the result records why, and the torrent stops being suspect.
func (e *Engine) VerifyAuto(ctx context.Context, hash, reason string) (*VerifyResult, error) {
	res, err := e.Verify(ctx, hash)
	if err != nil {
		return nil, err
	}
	if res.Unreadable > 0 {
		// Not verified: tried again later, still suspect meanwhile.
		return nil, fmt.Errorf("%d pieces couldn't be read", res.Unreadable)
	}
	en, err := e.find(hash)
	if err == nil {
		en.storage.ClearSuspect()
		e.mu.Lock()
		if en.verified != nil {
			r := *en.verified
			r.Reason = reason
			en.verified = &r
			res = &r
		}
		e.mu.Unlock()
	}
	return res, nil
}

// piecesBeingChecked reports whether any of t's pieces is queued for, or in,
// its hash check.
func piecesBeingChecked(t *torrent.Torrent) bool {
	for _, r := range t.PieceStateRuns() {
		if r.Checking || r.Hashing || r.QueuedForHash || r.Marking {
			return true
		}
	}
	return false
}
