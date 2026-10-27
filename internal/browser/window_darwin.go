//go:build darwin && cgo

package browser

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework WebKit -framework Network -framework Security
#include <stdlib.h>
#include "window_darwin.h"
*/
import "C"

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// Window is a Cocoa window with a WKWebView inside, every connection of
// which goes through Options.Proxy (macOS 14 and later: older WebKit can't
// be given a proxy of its own). One at a time, in the window's own process
// (see ServeHelper), on its main thread. Options.Args (Chromium switches)
// don't apply.
type Window struct {
	o       Options
	queue   chan func()
	closed  chan struct{}
	mu      sync.Mutex
	results map[int64]chan string
	nextID  atomic.Int64
}

var (
	openMu  sync.Mutex
	current *Window
)

// Open opens the window. It must be called on the main thread (the main
// goroutine, which Wails locks to it), and wait run there after it.
func Open(o Options) (*Window, error) {
	openMu.Lock()
	defer openMu.Unlock()
	if current != nil {
		return nil, errors.New("a browser window is already open")
	}
	w := &Window{o: o, queue: make(chan func(), 64), closed: make(chan struct{}), results: map[int64]chan string{}}
	// WebKit keeps a data store by ID; the profile's folder picks the ID.
	sum := sha256.Sum256([]byte(o.DataDir))
	sum[6], sum[8] = sum[6]&0x0f|0x40, sum[8]&0x3f|0x80 // a version 4 UUID
	store := fmt.Sprintf("%X-%X-%X-%X-%X", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])

	cs := func(s string) *C.char { return C.CString(s) }
	proxy, user, pass, st, title := cs(o.Proxy), cs(o.ProxyUser), cs(o.ProxyPass), cs(store), cs(o.Title)
	r := C.sbw_open_window(proxy, user, pass, st, title)
	for _, p := range []*C.char{proxy, user, pass, st, title} {
		C.free(unsafe.Pointer(p))
	}
	switch r {
	case 1:
		return nil, errors.New("links to file hosts' pages need macOS 14 or later (the browser window); direct file links and torrents work")
	case 2:
		return nil, errors.New("the browser window must open on the main thread")
	case 3:
		return nil, errors.New("the browser window can't turn off WebRTC on this version of macOS, so it doesn't open; direct file links and torrents work")
	case 4:
		return nil, errors.New("the browser window couldn't set up what it blocks")
	}
	current = w
	return w, nil
}

// wait runs the window until it's gone, on the main thread.
func (w *Window) wait() {
	C.sbw_run_app()
	openMu.Lock()
	current = nil
	openMu.Unlock()
	close(w.closed)
	if w.o.OnClosed != nil {
		w.o.OnClosed()
	}
}

// do runs fn on the window's thread; false if the window is gone.
func (w *Window) do(fn func()) bool {
	select {
	case <-w.closed:
		return false
	case w.queue <- fn:
		C.sbw_wakeup()
		return true
	}
}

//export goRunQueue
func goRunQueue() {
	w := cur()
	if w == nil {
		return
	}
	for {
		select {
		case fn := <-w.queue:
			fn()
		default:
			return
		}
	}
}

func cur() *Window {
	openMu.Lock()
	defer openMu.Unlock()
	return current
}

//export goLoaded
func goLoaded(uri *C.char) {
	if w := cur(); w != nil && w.o.OnLoaded != nil {
		w.o.OnLoaded(C.GoString(uri))
	}
}

//export goPopup
func goPopup(uri *C.char, user C.int) {
	if w := cur(); w != nil && w.o.OnPopup != nil {
		w.o.OnPopup(NewWindow{URL: C.GoString(uri), UserStarted: user != 0})
	}
}

//export goDownload
func goDownload(uri, disposition, mime *C.char, size C.int64_t) C.int {
	w := cur()
	if w == nil || w.o.OnDownload == nil {
		return 1 // nothing to hand it to: cancel
	}
	choice := w.o.OnDownload(Download{URL: C.GoString(uri), Disposition: C.GoString(disposition), MimeType: C.GoString(mime), Size: int64(size)})
	if choice.Cancel {
		return 1
	}
	return 0
}

//export goResult
func goResult(id C.int64_t, json *C.char) {
	w := cur()
	if w == nil {
		return
	}
	w.mu.Lock()
	ch := w.results[int64(id)]
	delete(w.results, int64(id))
	w.mu.Unlock()
	if ch != nil {
		ch <- C.GoString(json)
	}
}

// Done is closed once the window is gone.
func (w *Window) Done() <-chan struct{} { return w.closed }

func (w *Window) Navigate(url string) {
	w.do(func() {
		c := C.CString(url)
		C.sbw_load_uri(c)
		C.free(unsafe.Pointer(c))
	})
}

// Eval runs js in the page, ignoring its result.
func (w *Window) Eval(js string) {
	w.do(func() {
		c := C.CString(js)
		C.sbw_eval_js(c, 0)
		C.free(unsafe.Pointer(c))
	})
}

// Query runs js in the page and returns its result as JSON ("" if the
// window is gone, or no answer within 10 s).
func (w *Window) Query(js string) string {
	id := w.nextID.Add(1)
	got := make(chan string, 1)
	w.mu.Lock()
	w.results[id] = got
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.results, id)
		w.mu.Unlock()
	}()
	if !w.do(func() {
		c := C.CString(js)
		C.sbw_eval_js(c, C.int64_t(id))
		C.free(unsafe.Pointer(c))
	}) {
		return ""
	}
	select {
	case s := <-got:
		return s
	case <-w.closed:
		return ""
	case <-time.After(10 * time.Second):
		return ""
	}
}

// DocumentTitle is the page's <title> ("" if the window is gone).
func (w *Window) DocumentTitle() string {
	got := make(chan string, 1)
	if !w.do(func() { got <- C.GoString(C.sbw_page_title()) }) {
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
		c := C.CString(s)
		C.sbw_set_title(c)
		C.free(unsafe.Pointer(c))
	})
}

// Show brings the window to the front.
func (w *Window) Show() { w.do(func() { C.sbw_present() }) }

// Close closes the window (as if the user did) and waits until it's gone.
func (w *Window) Close() {
	if w.do(func() { C.sbw_close_window() }) {
		<-w.closed
	}
}
