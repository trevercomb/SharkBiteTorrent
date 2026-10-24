//go:build windows

package browser

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

// Options for a browser window.
type Options struct {
	// Proxy is the 127.0.0.1:port of a SOCKS5 proxy without a password
	// (Chromium can't log in to one) that sends everything through the VPN.
	Proxy string
	// DataDir holds the window's own browser profile (cookies, cache).
	DataDir string
	Title   string
	// Args are extra Chromium switches (e.g. features to turn off).
	Args []string

	// Called on the window's thread.
	OnDownload func(Download) DownloadChoice
	OnLoaded   func(url string)
	OnPopup    func(NewWindow)
	OnClosed   func() // the window is gone (closed by the user or Close)
}

// Window is a top-level window with a WebView2 inside. One at a time.
type Window struct {
	o        Options
	hwnd     uintptr
	chromium *edge.Chromium
	queue    chan func()
	closed   chan struct{}
}

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	ole32            = windows.NewLazySystemDLL("ole32.dll")
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	pRegisterClassEx = user32.NewProc("RegisterClassExW")
	pCreateWindowEx  = user32.NewProc("CreateWindowExW")
	pDefWindowProc   = user32.NewProc("DefWindowProcW")
	pDestroyWindow   = user32.NewProc("DestroyWindow")
	pGetMessage      = user32.NewProc("GetMessageW")
	pTranslate       = user32.NewProc("TranslateMessage")
	pDispatch        = user32.NewProc("DispatchMessageW")
	pShowWindow      = user32.NewProc("ShowWindow")
	pSetForeground   = user32.NewProc("SetForegroundWindow")
	pSetWindowText   = user32.NewProc("SetWindowTextW")
	pPostMessage     = user32.NewProc("PostMessageW")
	pPostQuit        = user32.NewProc("PostQuitMessage")
	pLoadIcon        = user32.NewProc("LoadIconW")
	pCoInit          = ole32.NewProc("CoInitializeEx")
	pCoUninit        = ole32.NewProc("CoUninitialize")
	pGetModule       = kernel32.NewProc("GetModuleHandleW")

	registerOnce sync.Once
	current      *Window // the open window, if any; used only on its thread
	openMu       sync.Mutex
)

const (
	wmDestroy = 0x0002
	wmSize    = 0x0005
	wmClose   = 0x0010
	wmApp     = 0x8000
	className = "SharkBiteTorrentBrowser"

	// ICoreWebView2 slots (see com_windows.go)
	slotNavigate         = 5
	slotExecuteScript    = 29
	slotGetDocumentTitle = 48
	// ICoreWebView2Controller.Close
	slotControllerClose = 24
)

type wndClassEx struct {
	size, style                uint32
	wndProc                    uintptr
	clsExtra, wndExtra         int32
	instance, icon, cursor, bg windows.Handle
	menuName, className        *uint16
	iconSm                     windows.Handle
}

type msg struct {
	hwnd           windows.Handle
	message        uint32
	wParam, lParam uintptr
	time           uint32
	pt             struct{ x, y int32 }
}

func wndProc(hwnd windows.Handle, m uint32, wp, lp uintptr) uintptr {
	w := current
	switch m {
	case wmApp:
		if w != nil {
			for len(w.queue) > 0 {
				(<-w.queue)()
			}
		}
		return 0
	case wmSize:
		if w != nil && w.chromium != nil {
			w.chromium.Resize()
		}
		return 0
	case wmClose:
		if w != nil && w.chromium != nil {
			if c := w.chromium.GetController(); c != nil {
				call(unsafe.Pointer(c), slotControllerClose) // ends WebView2's processes for this window
			}
		}
		pDestroyWindow.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		pPostQuit.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(uintptr(hwnd), uintptr(m), wp, lp)
	return r
}

// Open creates the window on a thread of its own and returns once WebView2
// is ready. Only one window can be open.
func Open(o Options) (*Window, error) {
	openMu.Lock()
	if current != nil {
		openMu.Unlock()
		return nil, errors.New("a browser window is already open")
	}
	w := &Window{o: o, queue: make(chan func(), 64), closed: make(chan struct{})}
	current = w
	openMu.Unlock()

	ready := make(chan error, 1)
	go w.run(ready)
	if err := <-ready; err != nil {
		openMu.Lock()
		current = nil
		openMu.Unlock()
		return nil, err
	}
	return w, nil
}

func (w *Window) run(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func() {
		openMu.Lock()
		current = nil
		openMu.Unlock()
		close(w.closed)
		if w.o.OnClosed != nil {
			w.o.OnClosed()
		}
	}()
	pCoInit.Call(0, 2) // COINIT_APARTMENTTHREADED
	defer pCoUninit.Call()

	inst, _, _ := pGetModule.Call(0)
	cls, _ := windows.UTF16PtrFromString(className)
	registerOnce.Do(func() {
		icon, _, _ := pLoadIcon.Call(inst, 3) // the app's own icon (resource 3, as Wails builds it)
		wc := wndClassEx{wndProc: windows.NewCallback(wndProc), instance: windows.Handle(inst), className: cls,
			bg: 6, icon: windows.Handle(icon), iconSm: windows.Handle(icon)}
		wc.size = uint32(unsafe.Sizeof(wc))
		pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	})
	title, _ := windows.UTF16PtrFromString(w.o.Title)
	const wsOverlappedWindow = 0x00CF0000
	const cwUseDefault = 0x80000000
	hwnd, _, err := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)), wsOverlappedWindow,
		cwUseDefault, cwUseDefault, 1100, 820, 0, 0, inst, 0)
	if hwnd == 0 {
		ready <- fmt.Errorf("couldn't create the browser window: %v", err)
		return
	}
	w.hwnd = hwnd
	pShowWindow.Call(hwnd, 5) // SW_SHOW

	c := edge.NewChromium()
	c.DataPath = w.o.DataDir
	c.AdditionalBrowserArgs = append([]string{
		"--proxy-server=socks5://" + w.o.Proxy,
		// Every name is resolved by the proxy (through the VPN), never here.
		`--host-resolver-rules="MAP * ~NOTFOUND , EXCLUDE 127.0.0.1"`,
		"--disable-quic", // UDP doesn't go through a SOCKS proxy
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
	}, w.o.Args...)
	c.SetErrorCallback(func(err error) {
		if Debug != nil {
			Debug("browser window: %v", err)
		}
	})
	c.NavigationCompletedCallback = func(sender *edge.ICoreWebView2, _ *edge.ICoreWebView2NavigationCompletedEventArgs) {
		if w.o.OnLoaded != nil {
			src, _ := sender.GetSource()
			w.o.OnLoaded(src)
		}
	}
	// The main window's switches (go-webview2's ExtraBrowserArgs, added
	// after ours) include a dead proxy that keeps it off the network; for
	// this window they'd win (Chromium uses the last copy of a switch).
	saved := edge.ExtraBrowserArgs
	edge.ExtraBrowserArgs = nil
	c.Embed(hwnd)
	edge.ExtraBrowserArgs = saved
	w.chromium = c
	c.Resize()

	if !OnNewWindow(c, func(nw NewWindow) {
		if w.o.OnPopup != nil {
			w.o.OnPopup(nw)
		}
	}) || !OnDownload(c, func(d Download) DownloadChoice {
		if w.o.OnDownload != nil {
			return w.o.OnDownload(d)
		}
		return DownloadChoice{Cancel: true}
	}) {
		pDestroyWindow.Call(hwnd)
		ready <- errors.New("this WebView2 version is too old for the browser window: update Microsoft Edge WebView2")
		return
	}
	ready <- nil

	var m msg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			return
		}
		pTranslate.Call(uintptr(unsafe.Pointer(&m)))
		pDispatch.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// do runs fn on the window's thread; false if the window is gone.
func (w *Window) do(fn func()) bool {
	select {
	case <-w.closed:
		return false
	case w.queue <- fn:
	}
	pPostMessage.Call(w.hwnd, wmApp, 0, 0)
	return true
}

// Done is closed once the window is gone.
func (w *Window) Done() <-chan struct{} { return w.closed }

func (w *Window) Navigate(url string) {
	w.do(func() {
		if wv := webviewOf(w.chromium); wv != nil {
			if p, err := windows.UTF16PtrFromString(url); err == nil {
				call(wv, slotNavigate, uintptr(unsafe.Pointer(p)))
			}
		}
	})
}

// Eval runs a script in the page; its result isn't returned.
func (w *Window) Eval(js string) {
	w.do(func() {
		if wv := webviewOf(w.chromium); wv != nil {
			if p, err := windows.UTF16PtrFromString(js); err == nil {
				call(wv, slotExecuteScript, uintptr(unsafe.Pointer(p)), 0)
			}
		}
	})
}

// DocumentTitle is the page's <title> ("" if the window is gone).
func (w *Window) DocumentTitle() string {
	got := make(chan string, 1)
	if !w.do(func() {
		t := ""
		if wv := webviewOf(w.chromium); wv != nil {
			t = getString(wv, slotGetDocumentTitle)
		}
		got <- t
	}) {
		return ""
	}
	select {
	case t := <-got:
		return t
	case <-w.closed:
		return ""
	}
}

func (w *Window) SetTitle(s string) {
	w.do(func() {
		if p, err := windows.UTF16PtrFromString(s); err == nil {
			pSetWindowText.Call(w.hwnd, uintptr(unsafe.Pointer(p)))
		}
	})
}

// Show brings the window to the front.
func (w *Window) Show() {
	w.do(func() {
		pShowWindow.Call(w.hwnd, 9) // SW_RESTORE
		pSetForeground.Call(w.hwnd)
	})
}

// Close closes the window (as if the user did) and waits until it's gone.
func (w *Window) Close() {
	if w.do(func() { pPostMessage.Call(w.hwnd, wmClose, 0, 0) }) {
		<-w.closed
	}
}
