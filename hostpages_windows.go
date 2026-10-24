//go:build windows

package main

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"sharkbite/internal/browser"
	"sharkbite/internal/socks"
)

// File hosts that only serve real browsers (behind Cloudflare, say) are
// opened in a browser window of the app's own, whose traffic goes through
// the VPN like everything else. The app visits each page in turn and clicks
// its download button itself, the way a userscript would; the download the
// page starts is cancelled in the window and handed to the app's own engine.
// Every pop-up is blocked. If a page asks for a human (a "verify you are
// human" check), the window says so and waits for the user: the app never
// tries to get around such a check.

// hostPage is a file host's page to get a download link from; ID is set when
// it's for a fresh link to an existing download.
type hostPage struct {
	URL string
	ID  string
}

type hostBrowser struct {
	win   *browser.Window
	proxy *socks.Server

	mu      sync.Mutex
	queue   []hostPage
	wake    chan struct{}
	loaded  chan string
	caught  chan caughtDownload
	stopped bool
}

type caughtDownload struct{ link, disposition string }

const (
	pageLoadWait   = 45 * time.Second
	clickTries     = 6
	clickWait      = 3 * time.Second
	userClickWait  = 3 * time.Minute
	humanCheckWait = 10 * time.Minute
)

// clickScript clicks the page's most prominent visible "download" link or
// button: an ordinary click() from a script.
const clickScript = `(() => {
	const els = [...document.querySelectorAll('a, button, input[type=button], input[type=submit]')]
		.filter(e => /download/i.test(e.textContent || e.value || '') && e.offsetParent !== null);
	const area = e => { const r = e.getBoundingClientRect(); return r.width * r.height; };
	els.sort((a, b) => area(b) - area(a));
	if (els.length) els[0].click();
})()`

func (a *App) openHostPages(pages []hostPage) error {
	a.hostMu.Lock()
	defer a.hostMu.Unlock()
	h := a.host
	if h == nil {
		var err error
		if h, err = a.startHostBrowser(); err != nil {
			return err
		}
		a.host = h
	}
	h.mu.Lock()
	h.queue = append(h.queue, pages...)
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
	return nil
}

func (a *App) startHostBrowser() (*hostBrowser, error) {
	if _, err := a.downloadClient(); err != nil {
		return nil, err // no VPN: nothing would load
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	h := &hostBrowser{
		wake:   make(chan struct{}, 1),
		loaded: make(chan string, 8),
		caught: make(chan caughtDownload, 8),
		proxy:  &socks.Server{NoAuth: true, Allow: browser.FromThisApp, Dial: a.searchDial},
	}
	go h.proxy.Serve(ln)
	h.win, err = browser.Open(browser.Options{
		Proxy:   ln.Addr().String(),
		DataDir: filepath.Join(a.configDir, "browser"),
		Title:   "Shark BiteTorrent: getting downloads",
		Args:    webviewQuietArgs,
		OnLoaded: func(url string) {
			select {
			case h.loaded <- url:
			default:
			}
		},
		OnDownload: func(d browser.Download) browser.DownloadChoice {
			select {
			case h.caught <- caughtDownload{d.URL, d.Disposition}:
			default:
			}
			return browser.DownloadChoice{Cancel: true} // the app downloads it itself
		},
	})
	if err != nil {
		h.proxy.Close()
		return nil, err
	}
	go a.driveHostBrowser(h)
	return h, nil
}

func (a *App) closeHostPages() {
	a.hostMu.Lock()
	h := a.host
	a.hostMu.Unlock()
	if h != nil {
		h.win.Close() // driveHostBrowser cleans up
	}
}

// driveHostBrowser works through the queue, then closes the window.
func (a *App) driveHostBrowser(h *hostBrowser) {
	defer func() {
		a.hostMu.Lock()
		if a.host == h {
			a.host = nil
		}
		a.hostMu.Unlock()
		h.win.Close()
		h.proxy.Close()
	}()
	done, total := 0, 0
	for {
		h.mu.Lock()
		if len(h.queue) == 0 {
			h.mu.Unlock()
			select {
			case <-h.wake:
				continue
			case <-time.After(3 * time.Second): // nothing more to do
				return
			case <-h.win.Done():
				return
			}
		}
		p := h.queue[0]
		h.queue = h.queue[1:]
		total = max(total, done+1+len(h.queue))
		h.mu.Unlock()

		done++
		h.win.SetTitle(fmt.Sprintf("Shark BiteTorrent: getting download %d of %d", done, total))
		if err := a.visitHostPage(h, p); err != nil {
			select {
			case <-h.win.Done():
				h.mu.Lock()
				left := len(h.queue) + 1
				h.mu.Unlock()
				a.notifyError(fmt.Errorf("the download window was closed: %d page(s) not done. Paste the links again to finish", left))
				return
			default:
			}
			a.notifyError(err)
		}
	}
}

func isHumanCheck(title string) bool {
	t := strings.ToLower(title)
	for _, s := range []string{"just a moment", "attention required", "checking your browser", "verify you are human", "ddos-guard"} {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

// visitHostPage loads one page and gets its download link.
func (a *App) visitHostPage(h *hostBrowser, p hostPage) error {
	drain := func() {
		for len(h.loaded) > 0 {
			<-h.loaded
		}
		for len(h.caught) > 0 {
			<-h.caught
		}
	}
	drain()
	h.win.Navigate(p.URL)
	select {
	case <-h.loaded:
	case <-time.After(pageLoadWait):
		return fmt.Errorf("%s didn't load", p.URL)
	case <-h.win.Done():
		return errWindowClosed
	}

	// A check that wants a human: the user does it, in the window.
	if isHumanCheck(h.win.DocumentTitle()) {
		h.win.SetTitle("Shark BiteTorrent: prove you're human in this window. The downloads carry on by themselves after that")
		h.win.Show()
		runtime.EventsEmit(a.ctx, "notice", "A file host wants you to prove you're human. Do that in the Shark BiteTorrent browser window, and the downloads carry on by themselves.")
		deadline := time.Now().Add(humanCheckWait)
		for isHumanCheck(h.win.DocumentTitle()) {
			if time.Now().After(deadline) {
				return fmt.Errorf("%s: the human check wasn't done in time", p.URL)
			}
			select {
			case <-time.After(time.Second):
			case <-h.win.Done():
				return errWindowClosed
			}
		}
	}

	time.Sleep(1500 * time.Millisecond) // let the page's own scripts set up
	for range clickTries {
		h.win.Eval(clickScript)
		select {
		case d := <-h.caught:
			a.addFromHost(p, d.link, d.disposition)
			return nil
		case <-time.After(clickWait):
		case <-h.win.Done():
			return errWindowClosed
		}
	}

	// The page didn't react to the app's clicks: the user clicks.
	h.win.SetTitle("Shark BiteTorrent: click this page's download button")
	h.win.Show()
	select {
	case d := <-h.caught:
		a.addFromHost(p, d.link, d.disposition)
		return nil
	case <-time.After(userClickWait):
		return fmt.Errorf("%s: no download started", p.URL)
	case <-h.win.Done():
		return errWindowClosed
	}
}

var errWindowClosed = fmt.Errorf("the download window was closed")
