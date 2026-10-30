//go:build windows || (linux && cgo) || (darwin && cgo)

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"sharkbite/internal/browser"
	"sharkbite/internal/paste"
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
	popups  chan struct{}   // a pop-up was blocked (an ad that took the click)
	stopped bool            // the app is quitting: closing the window ends everything
	opts    browser.Options // the window's, to open it again after a skipped page
}

type caughtDownload struct{ link, disposition string }

const (
	pageLoadWait     = 45 * time.Second
	clickTries       = 25               // two steps, clicks taken by ads, and a countdown: one host took 14 tries
	noButtonTries    = 4                // tries in a row that find no button, about 12 s
	noButtonUserWait = 30 * time.Second // then how long the user has to find one
	clickWait        = 3 * time.Second
	adClickWait      = time.Second // the click only opened an ad: the next one may do more
	userClickWait    = 3 * time.Minute
	humanCheckWait   = 10 * time.Minute
)

// clickScript clicks the page's free download link or button: an ordinary
// click() from a script. It never clicks anything that sells something (a
// host's premium offer is often its biggest "download" button), prefers
// "free download", then the most prominent "download". Disabled buttons
// (a countdown that hasn't finished) are left for the next try, and so is a
// countdown anywhere on the page ("Ready in 7s", "Preparing your
// download", sometimes next to a download button that still looks
// clickable): clicking during one can start the count again.
const clickScript = `(() => {
	const sells = /premium|pric|upgrade|buy|purchase|subscri|plan|pay|vip|full speed|fast download|high speed|sign ?up|register|log ?in|account|\bapp\b|desktop|extension|install|manager/i;
	const counting = /\b\d+\s*(s|sec|secs|seconds?|%)(\W|$)|please wait|preparing|generating/i;
	const text = e => (e.textContent || e.value || e.title || '').trim();
	// Links and buttons, and elements acting as buttons (role="button" or an
	// onclick handler); only short labels, as buttons have, so a page-wide
	// clickable wrapper (often an ad) is never the pick.
	const els = [...document.querySelectorAll('a, button, input[type=button], input[type=submit], [role=button], [onclick]')]
		.filter(e => e.offsetParent !== null && !e.disabled && e.getAttribute('aria-disabled') !== 'true' && /download/i.test(text(e)) && text(e).length < 80)
		.filter(e => !sells.test(text(e)) && !counting.test(text(e)) && !sells.test(e.getAttribute('href') || '') && !sells.test(String(e.className || '')));
	// A countdown or progress text that changed since the last try: wait.
	// Unchanged, it's instructions or a finished status ("Generating secure
	// link Done"), which mustn't stop the clicks.
	const shown = e => e.offsetParent !== null && e.getBoundingClientRect().width > 0;
	const counts = [...document.querySelectorAll('body *')].filter(e => e.children.length <= 3 && text(e).length < 80 && counting.test(text(e)) && shown(e)).map(text).join(' | ');
	const before = window.__sbtCounts;
	window.__sbtCounts = counts;
	if (counts && counts !== before) return 'counting down: ' + counts.slice(0, 80);
	const area = e => { const r = e.getBoundingClientRect(); return r.width * r.height; };
	const free = els.filter(e => /free/i.test(text(e)));
	const pick = (free.length ? free : els).sort((a, b) => area(b) - area(a))[0];
	if (!pick) return 'no download button among ' + els.length;
	// On screen, and the whole press, as a mouse sends it: some pages act on
	// the press or check the button is visible, which a bare click() isn't.
	pick.scrollIntoView({ block: 'center' });
	const r = pick.getBoundingClientRect(), at = { bubbles: true, cancelable: true, view: window, clientX: r.left + r.width / 2, clientY: r.top + r.height / 2 };
	for (const t of ['pointerdown', 'mousedown', 'pointerup', 'mouseup']) pick.dispatchEvent(new (t.startsWith('pointer') && window.PointerEvent ? PointerEvent : MouseEvent)(t, at));
	pick.click();
	return 'clicked ' + pick.tagName.toLowerCase() + ': ' + text(pick).slice(0, 60);
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
		popups: make(chan struct{}, 1),
		proxy:  &socks.Server{Dial: a.hostDial},
	}
	o := browser.Options{
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
		OnPopup: func(nw browser.NewWindow) {
			if u, err := url.Parse(nw.URL); err == nil {
				hostLog("pop-up blocked:", u.Host)
			}
			select {
			case h.popups <- struct{}{}:
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
	}
	browser.GuardProxy(h.proxy, &o)
	h.opts = o
	go h.proxy.Serve(ln)
	h.win, err = browser.Start(a.exe, o)
	if err != nil {
		h.proxy.Close()
		return nil, err
	}
	go a.driveHostBrowser(h)
	return h, nil
}

// hostDial is the window's way out: through the VPN (or directly with "No
// VPN"), but never to this computer or the home network, so a page can't
// reach a router's settings or a service listening on localhost. Every
// request goes through the proxy (checked on each OS, localhost included).
// A name is checked again once resolved, for names that point at 127.0.0.1;
// through the VPN, names resolve and connect inside the tunnel, where this
// computer isn't reachable anyway.
func (a *App) hostDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if localHost(host) {
		return nil, fmt.Errorf("%s is on this computer or the home network", host)
	}
	if !a.directMode() {
		return a.dialTunnel(ctx, network, addr)
	}
	return a.direct.dial(ctx, network, addr, true)
}

func (a *App) closeHostPages() {
	a.hostMu.Lock()
	h := a.host
	a.hostMu.Unlock()
	if h != nil {
		h.mu.Lock()
		h.stopped = true // not a skipped page: no window again
		w := h.win
		h.mu.Unlock()
		w.Close() // driveHostBrowser cleans up
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
				// Unless pages were queued just now: openHostPages queues
				// with hostMu held, so with it held an empty queue stays
				// empty, and from here new pages get a window of their own.
				a.hostMu.Lock()
				h.mu.Lock()
				more := len(h.queue) > 0
				h.mu.Unlock()
				if !more && a.host == h {
					a.host = nil
				}
				a.hostMu.Unlock()
				if more {
					continue
				}
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
		err := a.visitHostPage(h, p, status)
		if errors.Is(err, errPageSkipped) {
			// The user closed the window while it waited on them: that page
			// is skipped, and the window opens again for the rest.
			a.notifyError(fmt.Errorf("%s: skipped", paste.WithoutFragments(p.URL)))
			h.mu.Lock()
			more := len(h.queue) > 0 && !h.stopped
			h.mu.Unlock()
			if !more {
				return
			}
			w, err := browser.Start(a.exe, h.opts)
			if err != nil {
				a.notifyError(err)
				return
			}
			h.mu.Lock()
			h.win = w
			h.mu.Unlock()
			continue
		}
		if err != nil {
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
	// Only a box a person can see and click: ad scripts load invisible
	// reCAPTCHA frames (recaptcha/api2/aframe) that never get an answer.
	const visible = e => { const r = e.getBoundingClientRect(); return e.offsetParent !== null && r.width > 20 && r.height > 20; };
	const unsolved = (box, answer) => [...document.querySelectorAll(box)].some(visible) && !((document.querySelector(answer) || {}).value);
	return unsolved('iframe[src*="challenges.cloudflare.com"], .cf-turnstile', 'input[name="cf-turnstile-response"]')
		|| unsolved('iframe[src*="hcaptcha.com"], .h-captcha', 'textarea[name="h-captcha-response"]')
		|| unsolved('iframe[src*="recaptcha"], .g-recaptcha', 'textarea[name="g-recaptcha-response"]');
})()`

// hostDebug (SBT_LOG_LEVEL=debug) reports what each click did on stderr.
var hostDebug = os.Getenv("SBT_LOG_LEVEL") == "debug"

func hostLog(v ...any) {
	if hostDebug {
		fmt.Fprintln(os.Stderr, append([]any{time.Now().Format("15:04:05"), "file-host window:"}, v...)...)
	}
}

// humanGrace: many checks finish by themselves (no click) within a few
// seconds; only one still waiting after this asks the user.
const humanGrace = 6 * time.Second

// waitForHuman returns once the page isn't waiting for a person, asking the
// user to do the check in the window if it is. The app never tries to get
// around such a check.
func (a *App) waitForHuman(h *hostBrowser, p hostPage, status string) error {
	// A download already caught means nobody needs to prove anything (the
	// page started it by itself, or the check was passed): the caller takes it.
	pending := func() bool { return len(h.caught) == 0 && h.win.Query(humanCheckScript) == "true" }
	wait := func(until time.Time) (bool, error) {
		for pending() {
			hostLog("waiting on a human check")
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
		return fmt.Errorf("%s: the human check wasn't done in time", paste.WithoutFragments(p.URL))
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
		return fmt.Errorf("%s didn't load", paste.WithoutFragments(p.URL))
	case <-h.win.Done():
		return errWindowClosed
	}

	time.Sleep(1500 * time.Millisecond) // let the page's own scripts set up
	noButton := 0
	for range clickTries {
		// A page with nothing to click at all (no countdown either) isn't
		// going to grow a button: the user looks, rather than waiting a
		// minute for the app to give up.
		if noButton == noButtonTries {
			break
		}
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
		select {
		case <-h.popups:
		default:
		}
		r := h.win.Query(clickScript)
		hostLog(r) // diagnosing a page the clicks don't work on
		if strings.HasPrefix(r, `"no download button`) {
			noButton++
		} else {
			noButton = 0
		}
		select {
		case d := <-h.caught:
			a.addFromHost(p, d.link, d.disposition)
			return nil
		case <-h.popups:
			select {
			case d := <-h.caught:
				a.addFromHost(p, d.link, d.disposition)
				return nil
			case <-time.After(adClickWait):
			}
		case <-time.After(clickWait):
		case <-h.win.Done():
			return errWindowClosed
		}
	}

	// The page didn't react to the app's clicks: the user clicks. A page
	// with no download button at all likely has nothing to get (it isn't a
	// file host's page), so it's given up on sooner. Closing the window
	// now skips just this page.
	wait, title := userClickWait, "Shark BiteTorrent: click this page's download button, or close this window to skip it"
	if noButton == noButtonTries {
		wait = noButtonUserWait
		title = fmt.Sprintf("Shark BiteTorrent: no download button found. Click one if you see it; otherwise this page is skipped in %d s", int(wait.Seconds()))
	}
	h.win.SetTitle(title)
	h.win.Show()
	select {
	case d := <-h.caught:
		a.addFromHost(p, d.link, d.disposition)
		return nil
	case <-time.After(wait):
		if noButton == noButtonTries {
			return fmt.Errorf("%s: no download button on that page, so it was skipped", paste.WithoutFragments(p.URL))
		}
		return fmt.Errorf("%s: no download started", paste.WithoutFragments(p.URL))
	case <-h.win.Done():
		return errPageSkipped
	}
}

var (
	errWindowClosed = errors.New("the download window was closed")
	errPageSkipped  = errors.New("page skipped")
)

// browserWindowArg starts the app as the browser window's process.
const browserWindowArg = browser.HelperArg

// runBrowserWindow is that process: the window, run for the app that
// started it, until it closes or the app goes away.
func runBrowserWindow() error { return browser.ServeHelper(os.Stdin, os.Stdout) }
