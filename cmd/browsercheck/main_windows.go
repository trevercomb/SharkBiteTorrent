// browsercheck is a spike for "paste links": it opens a WebView2 window whose
// traffic goes through a WARP tunnel (SOCKS5 on 127.0.0.1), loads a page, and
// logs what the app could learn from it: the browser's User-Agent, the Cookie
// header the window sends, whether Go's own client can then fetch the page with
// that cookie, and links the page opens. Run: go run ./cmd/browsercheck <url>
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"

	"sharkbite/internal/browser"
	"sharkbite/internal/socks"
	"sharkbite/internal/tunnel"
	"sharkbite/internal/warp"
)

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	ole32            = windows.NewLazySystemDLL("ole32.dll")
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	pRegisterClassEx = user32.NewProc("RegisterClassExW")
	pCreateWindowEx  = user32.NewProc("CreateWindowExW")
	pDefWindowProc   = user32.NewProc("DefWindowProcW")
	pGetMessage      = user32.NewProc("GetMessageW")
	pTranslate       = user32.NewProc("TranslateMessage")
	pDispatch        = user32.NewProc("DispatchMessageW")
	pShowWindow      = user32.NewProc("ShowWindow")
	pPostQuit        = user32.NewProc("PostQuitMessage")
	pCoInit          = ole32.NewProc("CoInitializeEx")
	pGetModule       = kernel32.NewProc("GetModuleHandleW")
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

const (
	wmSize    = 0x0005
	wmDestroy = 0x0002
)

var chromium *edge.Chromium

func wndProc(hwnd windows.Handle, m uint32, w, l uintptr) uintptr {
	switch m {
	case wmSize:
		if chromium != nil {
			chromium.Resize()
		}
		return 0
	case wmDestroy:
		pPostQuit.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(uintptr(hwnd), uintptr(m), w, l)
	return r
}

func main() {
	if len(os.Args) != 2 {
		fmt.Println("usage: browsercheck <url>")
		os.Exit(2)
	}
	target := os.Args[1]

	dir, _ := os.UserConfigDir()
	acc, err := warp.LoadOrRegister(filepath.Join(dir, "SharkBiteTorrent", "warp.json"))
	if err != nil {
		fmt.Println("register:", err)
		os.Exit(1)
	}
	tun, err := tunnel.Start(tunnel.Config{PrivateKey: acc.PrivateKey, PeerPublicKey: acc.PeerPublicKey, Endpoint: acc.Endpoint,
		Addresses: []string{acc.AddressV4, acc.AddressV6}, DNS: []string{"1.1.1.1", "1.0.0.1"}})
	if err != nil {
		fmt.Println("tunnel:", err)
		os.Exit(1)
	}
	defer tun.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	var seen sync.Map
	srv := &socks.Server{NoAuth: true, Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if _, dup := seen.LoadOrStore(addr, true); !dup {
			fmt.Println("[proxy] connect", addr)
		}
		return tun.DialContext(ctx, network, addr)
	}}
	go srv.Serve(ln)
	defer srv.Close()

	runtime.LockOSThread()
	pCoInit.Call(0, 2) // COINIT_APARTMENTTHREADED
	inst, _, _ := pGetModule.Call(0)
	cls, _ := windows.UTF16PtrFromString("SharkBiteBrowserCheck")
	wc := wndClassEx{wndProc: windows.NewCallback(wndProc), instance: windows.Handle(inst), className: cls, bg: 6}
	wc.size = uint32(unsafe.Sizeof(wc))
	pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	title, _ := windows.UTF16PtrFromString("browsercheck")
	const wsOverlappedWindow = 0x00CF0000
	const cwUseDefault = 0x80000000
	hwnd, _, _ := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)), wsOverlappedWindow,
		cwUseDefault, cwUseDefault, 1100, 850, 0, 0, inst, 0)
	pShowWindow.Call(hwnd, 5)

	data, _ := os.MkdirTemp("", "sbt-browsercheck-")
	defer os.RemoveAll(data)
	chromium = edge.NewChromium()
	chromium.DataPath = data
	chromium.AdditionalBrowserArgs = []string{
		fmt.Sprintf("--proxy-server=socks5://%s", ln.Addr()),
		`--host-resolver-rules="MAP * ~NOTFOUND , EXCLUDE 127.0.0.1"`,
		"--disable-quic",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--disable-background-networking", "--disable-component-update", "--disable-domain-reliability",
		"--disable-sync", "--no-pings", "--metrics-recording-only", "--disable-breakpad",
	}
	chromium.SetErrorCallback(func(err error) { fmt.Println("[webview error]", err) })

	chromium.NavigationCompletedCallback = func(sender *edge.ICoreWebView2, _ *edge.ICoreWebView2NavigationCompletedEventArgs) {
		src, _ := sender.GetSource()
		fmt.Println("[navigated]", trim(src))
	}

	chromium.Embed(hwnd)
	chromium.Resize()
	ua := ""
	if st, err := chromium.GetSettings(); err == nil {
		ua, _ = st.GetUserAgent()
	}
	fmt.Println("[browser] user agent:", ua)
	browser.Debug = func(f string, a ...any) { fmt.Printf("[debug] "+f+"\n", a...) }
	fmt.Println("[browser] pop-up blocking:", browser.OnNewWindow(chromium, func(nw browser.NewWindow) {
		fmt.Printf("[blocked pop-up] %s (from a click: %v)\n", trim(nw.URL), nw.UserStarted)
	}))
	fmt.Println("[browser] download hook:", browser.OnDownload(chromium, func(d browser.Download) browser.DownloadChoice {
		fmt.Printf("[download] %s\n  type %s, size %d, disposition %q\n", trim(d.URL), d.MimeType, d.Size, d.Disposition)
		go tryGo(tun, d.URL, ua)
		return browser.DownloadChoice{Cancel: true}
	}))
	chromium.Navigate(target)

	var m msg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			break
		}
		pTranslate.Call(uintptr(unsafe.Pointer(&m)))
		pDispatch.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// tryGo fetches the first MB of a download the window started, with Go's own
// client through the same tunnel and the window's User-Agent (no cookies).
func tryGo(tun *tunnel.Tunnel, url, ua string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Range", "bytes=0-1048575")
	resp, err := tun.HTTPClient().Do(req)
	if err != nil {
		fmt.Println("[go client]", err)
		return
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, resp.Body)
	fmt.Printf("[go client] %s, %d bytes, Content-Range %q, type %q, challenge %q\n", resp.Status, n,
		resp.Header.Get("Content-Range"), resp.Header.Get("Content-Type"), resp.Header.Get("Cf-Mitigated"))
}

func trim(s string) string {
	if len(s) > 90 {
		return s[:90] + "…"
	}
	return s
}
