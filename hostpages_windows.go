//go:build windows

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
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
	URL    string
	ID     string
	Dir    string // where its download goes ("" = the default folder)
	Paused bool   // add the download paused
}

type hostBrowser struct {
	win   *browser.Remote // the window, in a process of its own
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
	clickTries     = 15 // a "free download" step and a countdown can take a while
	clickWait      = 3 * time.Second
	userClickWait  = 3 * time.Minute
	humanCheckWait = 10 * time.Minute
)

// clickScript clicks the page's free download link or button: an ordinary
// click() from a script. It never clicks anything that sells something (a
// host's premium offer is often its biggest "download" button), prefers
// "free download", then the most prominent "download". Disabled buttons
// (a countdown that hasn't finished) are left for the next try.
const clickScript = `(() => {
	const sells = /premium|pric|upgrade|buy|purchase|subscri|plan|pay|vip|full speed|fast download|high speed|sign ?up|register|log ?in|account|\bapp\b|desktop|extension|install|manager/i;
	const text = e => (e.textContent || e.value || e.title || '').trim();
	const els = [...document.querySelectorAll('a, button, input[type=button], input[type=submit]')]
		.filter(e => e.offsetParent !== null && !e.disabled && /download/i.test(text(e)))
		.filter(e => !sells.test(text(e)) && !sells.test(e.getAttribute('href') || '') && !sells.test(e.className || ''));
	const area = e => { const r = e.getBoundingClientRect(); return r.width * r.height; };
	const free = els.filter(e => /free/i.test(text(e)));
	const pick = (free.length ? free : els).sort((a, b) => area(b) - area(a))[0];
	if (pick) pick.click();
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
	h.win, err = browser.Start(a.exe, browser.Options{
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
		status := fmt.Sprintf("Shark BiteTorrent: getting download %d of %d", done, total)
		h.win.SetTitle(status)
		if err := a.visitHostPage(h, p, status); err != nil {
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

// humanCheckScript is true while the page waits for a person: a full-page
// check ("Just a moment…"), or a Cloudflare Turnstile, hCaptcha or reCAPTCHA
// box inside the page that hasn't been passed yet (each fills in a hidden
// field once it has).
const humanCheckScript = `(() => {
	if (/just a moment|attention required|checking your browser|verify you are human|ddos-guard/i.test(document.title)) return true;
	const unsolved = (box, answer) => !!document.querySelector(box) && !((document.querySelector(answer) || {}).value);
	return unsolved('iframe[src*="challenges.cloudflare.com"], .cf-turnstile', 'input[name="cf-turnstile-response"]')
		|| unsolved('iframe[src*="hcaptcha.com"], .h-captcha', 'textarea[name="h-captcha-response"]')
		|| unsolved('iframe[src*="recaptcha"], .g-recaptcha', 'textarea[name="g-recaptcha-response"]');
})()`

// humanGrace: many checks finish by themselves (no click) within a few
// seconds; only one still waiting after this asks the user.
const humanGrace = 6 * time.Second

// waitForHuman returns once the page isn't waiting for a person, asking the
// user to do the check in the window if it is. The app never tries to get
// around such a check.
func (a *App) waitForHuman(h *hostBrowser, p hostPage, status string) error {
	pending := func() bool { return h.win.Query(humanCheckScript) == "true" }
	wait := func(until time.Time) (bool, error) {
		for pending() {
			if time.Now().After(until) {
				return true, nil
			}
			select {
			case <-time.After(time.Second):
			case <-h.win.Done():
				return true, errWindowClosed
			}
		}
		return false, nil
	}
	still, err := wait(time.Now().Add(humanGrace))
	if !still || err != nil {
		return err
	}
	h.win.SetTitle("Shark BiteTorrent: prove you're human in this window. The downloads carry on by themselves after that")
	h.win.Show()
	runtime.EventsEmit(a.ctx, "notice", "A file host wants you to prove you're human. Do that in the Shark BiteTorrent browser window, and the downloads carry on by themselves.")
	if still, err = wait(time.Now().Add(humanCheckWait)); err != nil {
		return err
	} else if still {
		return fmt.Errorf("%s: the human check wasn't done in time", p.URL)
	}
	h.win.SetTitle(status)
	return nil
}

// visitHostPage loads one page and gets its download link. status is the
// window's title meanwhile.
func (a *App) visitHostPage(h *hostBrowser, p hostPage, status string) error {
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

	time.Sleep(1500 * time.Millisecond) // let the page's own scripts set up
	for range clickTries {
		// A check that wants a human (on loading, or after a "free
		// download" step): the user does it, in the window, before any click.
		if err := a.waitForHuman(h, p, status); err != nil {
			return err
		}
		select {
		case d := <-h.caught: // passing the check started the download
			a.addFromHost(p, d.link, d.disposition)
			return nil
		default:
		}
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

// browserWindowArg starts the app as the browser window's process.
const browserWindowArg = browser.HelperArg

// runBrowserWindow is that process: the window, run for the app that
// started it, until it closes or the app goes away.
func runBrowserWindow() error { return browser.ServeHelper(os.Stdin, os.Stdout) }
