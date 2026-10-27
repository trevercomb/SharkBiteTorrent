package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/time/rate"

	"sharkbite/internal/ddl"
	"sharkbite/internal/engine"
	"sharkbite/internal/paste"
	"sharkbite/internal/platform"
	"sharkbite/internal/tunnel"
)

// Direct downloads ("Paste links"): files fetched over HTTP(S) by
// internal/ddl, through the VPN like everything else. Links come from what
// the user pastes (text, a page, an encrypted PrivateBin paste); links to a
// file host's page go through the browser window (hostpages_*.go), which
// hands over the real download link.

type downloads struct {
	m       *ddl.Manager
	limiter *rate.Limiter
	save    chan struct{} // asks the saver to write downloads.json soon
	stop    context.CancelFunc

	clientMu  sync.Mutex
	clientTun *tunnel.Tunnel // the tunnel client was made for
	client    *http.Client
	direct    *http.Client
}

func (a *App) downloadsPath() string { return filepath.Join(a.configDir, "downloads.json") }

// startDownloads restores saved downloads; they wait until the VPN is up.
func (a *App) startDownloads() {
	d := &downloads{
		limiter: rate.NewLimiter(rate.Inf, 1<<20),
		save:    make(chan struct{}, 1),
	}
	d.m = &ddl.Manager{
		Client:      a.downloadClient,
		Limiter:     d.limiter,
		Connections: 4,
		UserAgent:   paste.UserAgent,
		OnChange: func() {
			select {
			case d.save <- struct{}{}:
			default:
			}
		},
	}
	a.mu.Lock()
	a.dl = d
	limit := a.settings.DownLimitKB
	a.mu.Unlock()
	a.setDownloadLimit(limit)

	var specs []ddl.Spec
	if data, err := os.ReadFile(a.downloadsPath()); err == nil {
		if err := json.Unmarshal(data, &specs); err != nil {
			runtime.LogErrorf(a.ctx, "downloads.json: %v", err)
		}
	}
	for _, s := range specs {
		if err := d.m.Add(s); err != nil {
			runtime.LogErrorf(a.ctx, "restore download: %v", err)
		}
	}
	go a.saveDownloadsLoop(d)
	ctx, cancel := context.WithCancel(context.Background())
	d.stop = cancel
	go a.downloadScanLoop(ctx, d.m)
}

// saveDownloadsLoop writes downloads.json at most every 2 s while things change.
func (a *App) saveDownloadsLoop(d *downloads) {
	for range d.save {
		time.Sleep(2 * time.Second)
		a.writeDownloads(d)
	}
}

func (a *App) writeDownloads(d *downloads) {
	data, _ := json.MarshalIndent(d.m.Specs(), "", "  ")
	if err := writeFile(a.downloadsPath(), data); err != nil {
		runtime.LogErrorf(a.ctx, "save downloads: %v", err)
	}
}

// stopDownloads stops them for quitting, keeping their progress.
func (a *App) stopDownloads() {
	a.mu.Lock()
	d := a.dl
	a.mu.Unlock()
	if d == nil {
		return
	}
	a.closeHostPages()
	d.stop()
	d.m.Close()
	a.writeDownloads(d)
}

func (a *App) setDownloadLimit(kb int64) {
	a.mu.Lock()
	d := a.dl
	a.mu.Unlock()
	if d == nil {
		return
	}
	if kb <= 0 {
		d.limiter.SetLimit(rate.Inf)
	} else {
		d.limiter.SetLimit(rate.Limit(kb * 1024))
	}
}

// downloadClient is the HTTP client for downloads: the VPN's, or the normal
// connection only when the user chose No VPN. Otherwise there's none, and
// downloads wait (ddl.ErrOffline).
func (a *App) downloadClient() (*http.Client, error) {
	a.mu.Lock()
	tun, d := a.tun, a.dl
	a.mu.Unlock()
	d.clientMu.Lock()
	defer d.clientMu.Unlock()
	if tun != nil {
		if d.clientTun != tun { // a new tunnel (reconnected): connections to the old one are dead
			d.clientTun = tun
			d.client = &http.Client{Transport: &http.Transport{
				DialContext:           tun.DialContext,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				MaxIdleConnsPerHost:   8,
			}}
		}
		return d.client, nil
	}
	if a.directMode() {
		if d.direct == nil {
			dialer := &net.Dialer{Timeout: 30 * time.Second, Resolver: &net.Resolver{}}
			d.direct = &http.Client{Transport: &http.Transport{
				DialContext:           dialer.DialContext,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
			}}
		}
		return d.direct, nil
	}
	return nil, errVPNDown
}

func (a *App) downloadManager() (*ddl.Manager, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dl == nil {
		return nil, errors.New("downloads aren't ready yet")
	}
	return a.dl.m, nil
}

func newDownloadID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ReadPaste reads what the user pasted (links, a page's address, an
// encrypted paste) through the VPN and lists the links in it.
func (a *App) ReadPaste(input string) ([]paste.Link, error) {
	hc, err := a.downloadClient()
	if err != nil {
		return nil, err
	}
	// The VPN can't reach this computer or the home network: say so at once
	// rather than after the minute a request there takes to give up.
	if u, err := url.Parse(strings.TrimSpace(input)); err == nil && localHost(u.Hostname()) && !a.directMode() {
		return nil, fmt.Errorf("%s is on this computer or your home network, which the app can't reach through the VPN", u.Hostname())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	links, err := paste.Find(ctx, hc, input)
	var he *paste.HTTPError
	if errors.As(err, &he) && he.Status >= 400 {
		// Many file hosts show their pages to browsers only (the app's own
		// request gets a 403 or 404): the pasted page itself goes to the
		// browser window, which gets its download.
		if l, ok := paste.PageLink(input); ok {
			return []paste.Link{l}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, errors.New("no links found in that")
	}
	return links, nil
}

type AddLinksResult struct {
	Torrents int      `json:"torrents"` // magnets and .torrent files added
	Files    int      `json:"files"`    // direct downloads started
	Pages    int      `json:"pages"`    // file-host pages opened in the browser window
	Errors   []string `json:"errors"`
}

// AddLinks downloads the links the user picked: torrents into the torrent
// list, files directly, and file-host pages through the browser window.
func (a *App) AddLinks(links []paste.Link) (AddLinksResult, error) {
	return a.addLinks(links, AddChoice{})
}

func (a *App) addLinks(links []paste.Link, choice AddChoice) (AddLinksResult, error) {
	res := AddLinksResult{Errors: []string{}}
	m, err := a.downloadManager()
	if err != nil {
		return res, err
	}
	dir := a.dirFor(choice)
	// Names can mislead both ways: a link that looks like a file may be a
	// file host's page (many put the file name in their page addresses), and
	// one that doesn't may be a file (a .txt, or a download script). Ask
	// each one for its first byte; only real files are downloaded directly.
	isFile := make(map[string]bool)
	var wg sync.WaitGroup
	var isFileMu sync.Mutex
	sem := make(chan struct{}, 6)
	for _, l := range links {
		if l.Kind == paste.File || l.Kind == paste.Page {
			wg.Add(1)
			go func(u string) {
				defer wg.Done()
				sem <- struct{}{}
				ok := a.servesFile(u)
				<-sem
				isFileMu.Lock()
				isFile[u] = ok
				isFileMu.Unlock()
			}(l.URL)
		}
	}
	wg.Wait()

	var pages []hostPage
	for _, l := range links {
		switch {
		case l.Kind == paste.File && !isFile[l.URL]:
			l.Kind = paste.Page
		case l.Kind == paste.Page && isFile[l.URL]:
			l.Kind = paste.File
		}
		var err error
		switch l.Kind {
		case paste.Magnet:
			if err = a.addSourceTo(l.URL, false, choice); err == nil {
				res.Torrents++
			}
		case paste.Torrent:
			if err = a.addTorrentURL(l.URL, choice); err == nil {
				res.Torrents++
			}
		case paste.File:
			if err = ensureDownloadDir(dir); err == nil {
				if err = m.Add(ddl.Spec{ID: newDownloadID(), URL: l.URL, Dir: dir, Name: l.Name, Paused: choice.Paused}); err == nil {
					res.Files++
				}
			}
		default:
			pages = append(pages, hostPage{URL: l.URL, Dir: dir, Paused: choice.Paused})
		}
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", engine.Redact(l.URL), err))
		}
	}
	if len(pages) > 0 {
		if err := ensureDownloadDir(dir); err != nil {
			return res, err
		}
		if err := a.openHostPages(pages); err != nil {
			res.Errors = append(res.Errors, err.Error())
		} else {
			res.Pages = len(pages)
		}
	}
	a.noteRecentDir(dir)
	return res, nil
}

// servesFile asks for a link's first byte: true if the answer is the file
// itself, false if it's a web page, a refusal (a file host's page, or a
// check that only a browser passes) or nothing at all.
func (a *App) servesFile(url string) bool {
	hc, err := a.downloadClient()
	if err != nil {
		return true // no connection to ask with: the download waits and tells later
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", paste.UserAgent)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent) && ct != "text/html"
}

// localHost is true for this computer and private network addresses
// (localhost, 127.x, 192.168.x, 10.x, fe80::, …), which the VPN can't reach.
func localHost(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified())
}

// addTorrentURL fetches a .torrent file through the VPN and adds it.
func (a *App) addTorrentURL(url string, choice AddChoice) error {
	hc, err := a.downloadClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", paste.UserAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.CreateTemp("", "sbt-*.torrent")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // addSource keeps its own copy
	_, err = io.Copy(f, io.LimitReader(resp.Body, 20<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return a.addSourceTo(f.Name(), false, choice)
}

// addFromHost starts (or, for a page already being downloaded, continues
// with) a download link the browser window got from a file host's page.
func (a *App) addFromHost(page hostPage, link, disposition string) {
	m, err := a.downloadManager()
	if err != nil {
		return
	}
	for _, s := range m.Specs() {
		if s.Page == page.URL && (page.ID == "" || page.ID == s.ID) {
			if !s.Done {
				m.SetURL(s.ID, link) // a fresh link for a download that has one already
			}
			return
		}
	}
	name := ""
	if _, params, err := mime.ParseMediaType(disposition); err == nil {
		name = params["filename"]
	}
	dir := page.Dir
	if dir == "" {
		dir = a.dirFor(AddChoice{})
	}
	if err := m.Add(ddl.Spec{ID: newDownloadID(), URL: link, Page: page.URL, Dir: dir, Name: name, Paused: page.Paused}); err != nil {
		a.notifyError(err)
	}
}

func (a *App) ListDownloads() []DownloadStatus {
	out := []DownloadStatus{}
	m, err := a.downloadManager()
	if err != nil {
		return out
	}
	specs := map[string]ddl.Spec{}
	for _, s := range m.Specs() {
		specs[s.ID] = s
	}
	for _, st := range m.Statuses() {
		out = append(out, a.downloadStatus(st, specs[st.ID]))
	}
	return out
}

func (a *App) PauseDownload(id string) error {
	m, err := a.downloadManager()
	if err == nil {
		m.Pause(id)
	}
	return err
}

func (a *App) ResumeDownload(id string) error {
	m, err := a.downloadManager()
	if err == nil {
		m.Resume(id)
	}
	return err
}

func (a *App) RemoveDownload(id string, deleteFile bool) error {
	m, err := a.downloadManager()
	if err != nil {
		return err
	}
	return m.Remove(id, deleteFile)
}

// GetNewLink opens a download's file-host page again for a fresh link (its
// old one stopped working).
func (a *App) GetNewLink(id string) error {
	m, err := a.downloadManager()
	if err != nil {
		return err
	}
	for _, s := range m.Specs() {
		if s.ID == id {
			if s.Page == "" {
				return errors.New("this download didn't come from a file host's page")
			}
			return a.openHostPages([]hostPage{{URL: s.Page, ID: id}})
		}
	}
	return errors.New("no such download")
}

// OpenDownloadFolder shows a download's file (or its folder, while it's
// still downloading) in the file manager.
func (a *App) OpenDownloadFolder(id string) error {
	m, err := a.downloadManager()
	if err != nil {
		return err
	}
	for _, s := range m.Specs() {
		if s.ID != id {
			continue
		}
		if s.Done {
			if p := filepath.Join(s.Dir, s.Name); exists(p) {
				return platform.RevealPath(p, false)
			}
		}
		return platform.OpenFolder(s.Dir)
	}
	return errors.New("no such download")
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
