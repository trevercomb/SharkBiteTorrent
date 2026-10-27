//go:build windows

// Package browser opens a small WebView2 window whose traffic goes through the
// VPN, for file hosts that only serve real browsers. The app takes over the
// downloads the page starts and blocks every pop-up it tries to open.
package browser

import (
	"sync"
	"syscall"
	"unsafe"

	"github.com/wailsapp/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

// go-webview2 doesn't cover the WebView2 events this needs (download
// starting, new window requested), so they're called here directly. Slot
// numbers count from the start of each interface's vtable (QueryInterface is
// 0) and follow WebView2.h: ICoreWebView2 has 58 methods after IUnknown,
// ICoreWebView2_2 adds 7, _3 adds 5, then _4.
const (
	slotAddNewWindowRequested = 44 // ICoreWebView2
	slotAddDownloadStarting   = 75 // ICoreWebView2_4

	// ICoreWebView2NewWindowRequestedEventArgs
	slotNewWindowGetURI        = 3
	slotNewWindowPutHandled    = 6
	slotNewWindowIsUserStarted = 8

	// ICoreWebView2DownloadStartingEventArgs
	slotDownloadGetOperation = 3
	slotDownloadPutCancel    = 5
	slotDownloadPutPath      = 7
	slotDownloadPutHandled   = 9

	// ICoreWebView2DownloadOperation
	slotOpGetURI         = 9
	slotOpGetDisposition = 10
	slotOpGetMimeType    = 11
	slotOpGetTotal       = 12
	slotOpCancel         = 18
)

// Debug, if set, gets diagnostic messages.
var Debug func(format string, args ...any)

var iidICoreWebView2_4 = windows.GUID{Data1: 0x20d02d59, Data2: 0x6df2, Data3: 0x42dc, Data4: [8]byte{0xbd, 0x06, 0xf9, 0x8a, 0x69, 0x4b, 0x13, 0x02}}

var procCoTaskMemFree = windows.NewLazySystemDLL("ole32.dll").NewProc("CoTaskMemFree")

// call invokes method slot of the COM object obj.
func call(obj unsafe.Pointer, slot int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

func failed(hr uintptr) bool { return int32(hr) < 0 }

// getString calls a getter returning an LPWSTR the caller must free.
func getString(obj unsafe.Pointer, slot int) string {
	var p *uint16
	if failed(call(obj, slot, uintptr(unsafe.Pointer(&p)))) || p == nil {
		return ""
	}
	s := windows.UTF16PtrToString(p)
	procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	return s
}

func putBool(obj unsafe.Pointer, slot int, v bool) {
	b := uintptr(0)
	if v {
		b = 1
	}
	call(obj, slot, b)
}

// handler is a COM event handler: IUnknown plus Invoke(sender, args). The
// objects live in Go memory for the window's life (kept in handlers), and
// QueryInterface answers for any interface, which is all WebView2 asks of
// an event handler.
type handler struct {
	vtbl   *[4]uintptr
	invoke func(args unsafe.Pointer)
}

var (
	handlerVtbl = [4]uintptr{
		windows.NewCallback(func(this *handler, _ *windows.GUID, out *unsafe.Pointer) uintptr {
			*out = unsafe.Pointer(this)
			return 0
		}),
		windows.NewCallback(func(*handler) uintptr { return 1 }),
		windows.NewCallback(func(*handler) uintptr { return 1 }),
		windows.NewCallback(func(this *handler, _, args unsafe.Pointer) uintptr {
			this.invoke(args)
			return 0
		}),
	}
	handlersMu sync.Mutex
	handlers   []*handler // keeps them reachable while WebView2 holds them
)

func newHandler(fn func(args unsafe.Pointer)) *handler {
	h := &handler{vtbl: &handlerVtbl, invoke: fn}
	handlersMu.Lock()
	handlers = append(handlers, h)
	handlersMu.Unlock()
	return h
}

// forget drops a one-shot handler once WebView2 has called it.
func forget(h *handler) {
	handlersMu.Lock()
	defer handlersMu.Unlock()
	for i, x := range handlers {
		if x == h {
			handlers = append(handlers[:i], handlers[i+1:]...)
			return
		}
	}
}

// executeScript runs js in the page and calls done with its result as JSON
// ("null" if it failed). ExecuteScript's completion handler is called with
// (HRESULT, LPCWSTR result), which fits handler's (sender, args).
func executeScript(wv unsafe.Pointer, js string, done func(string)) bool {
	p, err := windows.UTF16PtrFromString(js)
	if err != nil {
		return false
	}
	var h *handler
	h = newHandler(func(result unsafe.Pointer) {
		forget(h)
		s := "null"
		if result != nil {
			s = windows.UTF16PtrToString((*uint16)(result))
		}
		done(s)
	})
	if failed(call(wv, slotExecuteScript, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(h)))) {
		forget(h)
		return false
	}
	return true
}

func webviewOf(c *edge.Chromium) unsafe.Pointer {
	wv, err := c.GetController().GetCoreWebView2()
	if err != nil || wv == nil {
		return nil
	}
	return unsafe.Pointer(wv)
}

// OnNewWindow blocks every new window the page opens and reports it.
func OnNewWindow(c *edge.Chromium, fn func(NewWindow)) bool {
	wv := webviewOf(c)
	if wv == nil {
		return false
	}
	h := newHandler(func(args unsafe.Pointer) {
		var user int32
		call(args, slotNewWindowIsUserStarted, uintptr(unsafe.Pointer(&user)))
		nw := NewWindow{URL: getString(args, slotNewWindowGetURI), UserStarted: user != 0}
		putBool(args, slotNewWindowPutHandled, true) // handled, with no window given: nothing opens
		if fn != nil {
			fn(nw)
		}
	})
	var token int64
	return !failed(call(wv, slotAddNewWindowRequested, uintptr(unsafe.Pointer(h)), uintptr(unsafe.Pointer(&token))))
}

// OnDownload reports each download the page starts; the browser's own
// download panel stays hidden either way. Needs WebView2 runtime 92 or later.
func OnDownload(c *edge.Chromium, fn func(Download) DownloadChoice) bool {
	wv := webviewOf(c)
	if wv == nil {
		return false
	}
	var wv4 unsafe.Pointer
	if failed(call(wv, 0, uintptr(unsafe.Pointer(&iidICoreWebView2_4)), uintptr(unsafe.Pointer(&wv4)))) || wv4 == nil {
		return false
	}
	h := newHandler(func(args unsafe.Pointer) {
		var op unsafe.Pointer
		if failed(call(args, slotDownloadGetOperation, uintptr(unsafe.Pointer(&op)))) || op == nil {
			return
		}
		d := Download{
			URL:         getString(op, slotOpGetURI),
			Disposition: getString(op, slotOpGetDisposition),
			MimeType:    getString(op, slotOpGetMimeType),
			Size:        -1,
		}
		call(op, slotOpGetTotal, uintptr(unsafe.Pointer(&d.Size)))
		defer call(op, 2) // Release
		choice := fn(d)
		hrH := call(args, slotDownloadPutHandled, 1)
		if choice.Cancel {
			hrC := call(args, slotDownloadPutCancel, 1)
			var gotC, gotH int32
			call(args, slotDownloadPutCancel-1, uintptr(unsafe.Pointer(&gotC)))
			call(args, slotDownloadPutHandled-1, uintptr(unsafe.Pointer(&gotH)))
			hrOp := call(op, slotOpCancel)
			if Debug != nil {
				Debug("put_Handled %#x (now %d), put_Cancel %#x (now %d), operation Cancel %#x", hrH, gotH, hrC, gotC, hrOp)
			}
		} else if choice.Path != "" {
			if p, err := windows.UTF16PtrFromString(choice.Path); err == nil {
				call(args, slotDownloadPutPath, uintptr(unsafe.Pointer(p)))
			}
		}
	})
	var token int64
	ok := !failed(call(wv4, slotAddDownloadStarting, uintptr(unsafe.Pointer(h)), uintptr(unsafe.Pointer(&token))))
	call(wv4, 2) // Release
	return ok
}
