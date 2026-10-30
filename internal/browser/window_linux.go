//go:build linux && cgo

package browser

/*
#cgo pkg-config: webkit2gtk-4.1 gtk+-3.0
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <gtk/gtk.h>
#include <webkit2/webkit2.h>

extern void goRunQueue(void);
extern void goLoaded(char *uri);
extern void goPopup(char *uri, int user);
extern int goDownload(char *uri, char *disposition, char *mime, gint64 size);
extern void goResult(gint64 id, char *json);
extern void goClosed(void);

static GtkWidget *win;
static WebKitWebView *view;

static gboolean idle_cb(gpointer d) { goRunQueue(); return G_SOURCE_REMOVE; }
static void wakeup(void) { g_idle_add(idle_cb, NULL); }

static const char *orempty(const char *s) { return s ? s : ""; }

static void on_load_changed(WebKitWebView *v, WebKitLoadEvent e, gpointer d) {
	if (e == WEBKIT_LOAD_FINISHED) goLoaded((char *)orempty(webkit_web_view_get_uri(v)));
}

// The stand-in for a pop-up: never shown, every navigation in it refused
// (nothing loads), thrown away after a while.
static gboolean deny_all(WebKitWebView *v, WebKitPolicyDecision *dec, WebKitPolicyDecisionType t, gpointer d) {
	webkit_policy_decision_ignore(dec);
	return TRUE;
}

static gboolean drop_standin(gpointer w) {
	gtk_widget_destroy(GTK_WIDGET(w));
	g_object_unref(w);
	return G_SOURCE_REMOVE;
}

// A pop-up the page asked for: reported, and nothing opens. The page gets a
// hidden stand-in window rather than none, as WebView2's handled request
// does on Windows: an ad script that hijacks the download button's first
// clicks to open an ad lets the next click through once it believes its ad
// opened; given nothing, it hijacked every click.
static GtkWidget *on_create(WebKitWebView *v, WebKitNavigationAction *a, gpointer d) {
	WebKitURIRequest *r = webkit_navigation_action_get_request(a);
	goPopup((char *)orempty(r ? webkit_uri_request_get_uri(r) : NULL), webkit_navigation_action_is_user_gesture(a));
	GtkWidget *standin = webkit_web_view_new_with_related_view(v);
	g_object_ref_sink(standin);
	g_signal_connect(standin, "decide-policy", G_CALLBACK(deny_all), NULL);
	g_timeout_add_seconds(60, drop_standin, standin);
	return standin;
}

static int report_download(WebKitURIResponse *resp) {
	SoupMessageHeaders *h = webkit_uri_response_get_http_headers(resp);
	const char *cd = h ? soup_message_headers_get_one(h, "Content-Disposition") : NULL;
	return goDownload((char *)orempty(webkit_uri_response_get_uri(resp)), (char *)orempty(cd),
		(char *)orempty(webkit_uri_response_get_mime_type(resp)), (gint64)webkit_uri_response_get_content_length(resp));
}

// A response it can't show, or one sent as an attachment, is a download,
// from the page or any frame in it (pages often start theirs from a hidden
// frame), as WebView2 reports them on Windows.
static gboolean on_decide_policy(WebKitWebView *v, WebKitPolicyDecision *dec, WebKitPolicyDecisionType t, gpointer d) {
	if (t == WEBKIT_POLICY_DECISION_TYPE_NEW_WINDOW_ACTION) {
		WebKitNavigationAction *a = webkit_navigation_policy_decision_get_navigation_action(WEBKIT_NAVIGATION_POLICY_DECISION(dec));
		WebKitURIRequest *r = webkit_navigation_action_get_request(a);
		goPopup((char *)orempty(r ? webkit_uri_request_get_uri(r) : NULL), webkit_navigation_action_is_user_gesture(a));
		webkit_policy_decision_ignore(dec);
		return TRUE;
	}
	if (t == WEBKIT_POLICY_DECISION_TYPE_NAVIGATION_ACTION) {
		// Web pages only: no other app's links (magnet:, mailto:…), which
		// would hand them to a program outside the VPN.
		WebKitNavigationAction *a = webkit_navigation_policy_decision_get_navigation_action(WEBKIT_NAVIGATION_POLICY_DECISION(dec));
		WebKitURIRequest *r = webkit_navigation_action_get_request(a);
		const char *u = r ? webkit_uri_request_get_uri(r) : NULL;
		char *scheme = u ? g_uri_parse_scheme(u) : NULL;
		gboolean web = scheme && (!g_ascii_strcasecmp(scheme, "http") || !g_ascii_strcasecmp(scheme, "https") ||
			!g_ascii_strcasecmp(scheme, "about") || !g_ascii_strcasecmp(scheme, "data") || !g_ascii_strcasecmp(scheme, "blob"));
		g_free(scheme);
		if (!web) {
			webkit_policy_decision_ignore(dec);
			return TRUE;
		}
	}
	if (t == WEBKIT_POLICY_DECISION_TYPE_RESPONSE) {
		WebKitResponsePolicyDecision *rd = WEBKIT_RESPONSE_POLICY_DECISION(dec);
		WebKitURIResponse *resp = webkit_response_policy_decision_get_response(rd);
		SoupMessageHeaders *h = webkit_uri_response_get_http_headers(resp);
		const char *cd = h ? soup_message_headers_get_one(h, "Content-Disposition") : NULL;
		gboolean attachment = cd && g_ascii_strncasecmp(cd, "attachment", 10) == 0;
		if (attachment || !webkit_response_policy_decision_is_mime_type_supported(rd)) {
			if (report_download(resp)) webkit_policy_decision_ignore(dec); // the app downloads it itself
			else webkit_policy_decision_download(dec);
			return TRUE;
		}
	}
	return FALSE;
}

// Downloads that don't go through a response decision (<a download>).
static void on_download_started(WebKitWebContext *c, WebKitDownload *dl, gpointer d) {
	WebKitURIRequest *r = webkit_download_get_request(dl);
	goDownload((char *)orempty(r ? webkit_uri_request_get_uri(r) : NULL), "", "", -1);
	webkit_download_cancel(dl);
}

static void on_destroy(GtkWidget *w, gpointer d) {
	view = NULL;
	win = NULL;
	gtk_main_quit();
}

// What the window never loads, as on macOS (window_darwin.m): anything on
// this computer or the home network (WebKitGTK sends them to the proxy,
// which refuses them, but that's not something to rely on), and audio or
// video. WebKit's content blocker rules, compiled once into the profile.
static const char *block_rules = "["
	"{\"trigger\":{\"url-filter\":\"^[a-z-]+://localhost[:/]\"},\"action\":{\"type\":\"block\"}},"
	"{\"trigger\":{\"url-filter\":\"^[a-z-]+://[^/]*\\\\.localhost[:/]\"},\"action\":{\"type\":\"block\"}},"
	"{\"trigger\":{\"url-filter\":\"^[a-z-]+://[^/]*\\\\.local[:/]\"},\"action\":{\"type\":\"block\"}},"
	"{\"trigger\":{\"url-filter\":\"^[a-z-]+://127\\\\.\"},\"action\":{\"type\":\"block\"}},"
	"{\"trigger\":{\"url-filter\":\"^[a-z-]+://0\\\\.\"},\"action\":{\"type\":\"block\"}},"
	"{\"trigger\":{\"url-filter\":\"^[a-z-]+://10\\\\.\"},\"action\":{\"type\":\"block\"}},"
	"{\"trigger\":{\"url-filter\":\"^[a-z-]+://192\\\\.168\\\\.\"},\"action\":{\"type\":\"block\"}},"
	"{\"trigger\":{\"url-filter\":\"^[a-z-]+://169\\\\.254\\\\.\"},\"action\":{\"type\":\"block\"}},"
	"{\"trigger\":{\"url-filter\":\"^[a-z-]+://172\\\\.(1[6-9])\\\\.\"},\"action\":{\"type\":\"block\"}},"
	"{\"trigger\":{\"url-filter\":\"^[a-z-]+://172\\\\.(2[0-9])\\\\.\"},\"action\":{\"type\":\"block\"}},"
	"{\"trigger\":{\"url-filter\":\"^[a-z-]+://172\\\\.(3[01])\\\\.\"},\"action\":{\"type\":\"block\"}},"
	"{\"trigger\":{\"url-filter\":\"^[a-z-]+://\\\\[\"},\"action\":{\"type\":\"block\"}},"
	"{\"trigger\":{\"url-filter\":\".*\",\"resource-type\":[\"media\"]},\"action\":{\"type\":\"block\"}}"
	"]";

static WebKitUserContentFilter *rules_filter;
static gboolean rules_done;

static void rules_saved(GObject *store, GAsyncResult *res, gpointer d) {
	rules_filter = webkit_user_content_filter_store_save_finish(WEBKIT_USER_CONTENT_FILTER_STORE(store), res, NULL);
	rules_done = TRUE;
}

// load_rules compiles the block rules (on this thread, before anything
// loads); NULL if they didn't compile.
static WebKitUserContentFilter *load_rules(const char *datadir) {
	char *path = g_build_filename(datadir, "content-filters", NULL);
	WebKitUserContentFilterStore *store = webkit_user_content_filter_store_new(path);
	g_free(path);
	GBytes *src = g_bytes_new_static(block_rules, strlen(block_rules));
	rules_done = FALSE;
	webkit_user_content_filter_store_save(store, "sharkbite-browser-window", src, NULL, rules_saved, NULL);
	while (!rules_done) g_main_context_iteration(NULL, TRUE);
	g_bytes_unref(src);
	g_object_unref(store);
	return rules_filter;
}

static int open_window(const char *proxy, const char *datadir, const char *title) {
	g_set_prgname("sharkbitetorrent"); // the app's icon (the Wayland app ID names its menu entry)
	if (!gtk_init_check(NULL, NULL)) return 0;
	char *cache = g_build_filename(datadir, "cache", NULL);
	WebKitWebsiteDataManager *m = webkit_website_data_manager_new(
		"base-data-directory", datadir, "base-cache-directory", cache, NULL);
	g_free(cache);
	// Everything through the app's SOCKS5 proxy: host names go to it
	// unresolved (GLib's SOCKS5 client sends the name), so they're looked up
	// inside the VPN. No exceptions (no ignore list).
	char *uri = g_strdup_printf("socks5://%s", proxy);
	const char *none[] = { NULL };
	WebKitNetworkProxySettings *ps = webkit_network_proxy_settings_new(uri, none);
	webkit_website_data_manager_set_network_proxy_settings(m, WEBKIT_NETWORK_PROXY_MODE_CUSTOM, ps);
	webkit_network_proxy_settings_free(ps);
	g_free(uri);
	WebKitWebContext *ctx = webkit_web_context_new_with_website_data_manager(m);
	g_signal_connect(ctx, "download-started", G_CALLBACK(on_download_started), NULL);

	view = WEBKIT_WEB_VIEW(webkit_web_view_new_with_context(ctx));
	WebKitUserContentFilter *rules = load_rules(datadir);
	if (!rules) {
		g_object_unref(g_object_ref_sink(view));
		view = NULL;
		return 0;
	}
	webkit_user_content_manager_add_filter(webkit_web_view_get_user_content_manager(view), rules);
	webkit_user_content_filter_unref(rules);
	WebKitSettings *s = webkit_web_view_get_settings(view);
	// Nothing that could look names up or connect around the proxy.
	// Deprecated since WebKit 2.48 (may do nothing now); the Linux leak
	// test is what shows no name is looked up outside the proxy.
	G_GNUC_BEGIN_IGNORE_DEPRECATIONS
	webkit_settings_set_enable_dns_prefetching(s, FALSE);
	G_GNUC_END_IGNORE_DEPRECATIONS
	webkit_settings_set_enable_webrtc(s, FALSE);
	webkit_settings_set_enable_media_stream(s, FALSE);
	// No audio or video at all (as on macOS): a download page needs none,
	// and an <audio> element made WebKit set up GStreamer, which hung the
	// page where GStreamer's audio output plugin isn't installed.
	webkit_settings_set_enable_media(s, FALSE);
	webkit_settings_set_enable_webaudio(s, FALSE);
	webkit_settings_set_enable_mediasource(s, FALSE);
	webkit_settings_set_enable_encrypted_media(s, FALSE);
	// Pop-ups reach on_create (which never opens one): refused outright, an
	// ad script that hijacks the download button's click to open one keeps
	// retrying on every click.
	webkit_settings_set_javascript_can_open_windows_automatically(s, TRUE);
	// No GPU rendering, as in the app's main window (Wails does the same):
	// on Wayland, WebKitGTK's DMA-BUF renderer killed the window with
	// "Error 71 (Protocol error)" on this PC (NVIDIA).
	webkit_settings_set_hardware_acceleration_policy(s, WEBKIT_HARDWARE_ACCELERATION_POLICY_NEVER);
	g_signal_connect(view, "load-changed", G_CALLBACK(on_load_changed), NULL);
	g_signal_connect(view, "create", G_CALLBACK(on_create), NULL);
	g_signal_connect(view, "decide-policy", G_CALLBACK(on_decide_policy), NULL);

	win = gtk_window_new(GTK_WINDOW_TOPLEVEL);
	gtk_window_set_title(GTK_WINDOW(win), title);
	gtk_window_set_default_size(GTK_WINDOW(win), 1100, 760);
	gtk_container_add(GTK_CONTAINER(win), GTK_WIDGET(view));
	g_signal_connect(win, "destroy", G_CALLBACK(on_destroy), NULL);
	gtk_widget_show_all(win);
	return 1;
}

static void js_done(GObject *o, GAsyncResult *res, gpointer data) {
	gint64 id = (gint64)(intptr_t)data;
	GError *err = NULL;
#if WEBKIT_CHECK_VERSION(2, 40, 0)
	JSCValue *v = webkit_web_view_evaluate_javascript_finish(WEBKIT_WEB_VIEW(o), res, &err);
#else
	WebKitJavascriptResult *jr = webkit_web_view_run_javascript_finish(WEBKIT_WEB_VIEW(o), res, &err);
	JSCValue *v = jr ? g_object_ref(webkit_javascript_result_get_js_value(jr)) : NULL;
	if (jr) webkit_javascript_result_unref(jr);
#endif
	if (!v) {
		g_clear_error(&err);
		if (id) goResult(id, "");
		return;
	}
	if (id) {
		char *s = jsc_value_to_json(v, 0);
		goResult(id, s ? s : "null");
		g_free(s);
	}
	g_object_unref(v);
}

static void eval_js(const char *js, gint64 id) {
	if (!view) {
		if (id) goResult(id, "");
		return;
	}
#if WEBKIT_CHECK_VERSION(2, 40, 0)
	webkit_web_view_evaluate_javascript(view, js, -1, NULL, NULL, NULL, js_done, (gpointer)(intptr_t)id);
#else
	webkit_web_view_run_javascript(view, js, NULL, js_done, (gpointer)(intptr_t)id);
#endif
}

static void load_uri(const char *u) { if (view) webkit_web_view_load_uri(view, u); }
static const char *page_title(void) { return view ? orempty(webkit_web_view_get_title(view)) : ""; }
static void set_title(const char *t) { if (win) gtk_window_set_title(GTK_WINDOW(win), t); }
static void present(void) { if (win) gtk_window_present(GTK_WINDOW(win)); }
static void destroy_window(void) { if (win) gtk_widget_destroy(win); }
*/
import "C"

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// Window is a GTK window with a WebKitGTK view inside, every connection of
// which goes through Options.Proxy. One at a time, in the window's own
// process (see ServeHelper). Options.Args (Chromium switches) don't apply.
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

func Open(o Options) (*Window, error) {
	openMu.Lock()
	if current != nil {
		openMu.Unlock()
		return nil, errors.New("a browser window is already open")
	}
	w := &Window{o: o, queue: make(chan func(), 64), closed: make(chan struct{}), results: map[int64]chan string{}}
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
	// GTK runs on the thread that started it, for the window's whole life.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	proxy, dir, title := C.CString(w.o.Proxy), C.CString(w.o.DataDir), C.CString(w.o.Title)
	ok := C.open_window(proxy, dir, title) != 0
	C.free(unsafe.Pointer(proxy))
	C.free(unsafe.Pointer(dir))
	C.free(unsafe.Pointer(title))
	if !ok {
		ready <- errors.New("couldn't open the browser window (no display?)")
		return
	}
	ready <- nil
	C.gtk_main()
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
		C.wakeup()
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
func goDownload(uri, disposition, mime *C.char, size C.gint64) C.int {
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
func goResult(id C.gint64, json *C.char) {
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

//export goClosed
func goClosed() {}

// wait waits until the window is gone.
func (w *Window) wait() { <-w.closed }

// Done is closed once the window is gone.
func (w *Window) Done() <-chan struct{} { return w.closed }

func (w *Window) Navigate(url string) {
	w.do(func() {
		c := C.CString(url)
		C.load_uri(c)
		C.free(unsafe.Pointer(c))
	})
}

// Eval runs js in the page, ignoring its result.
func (w *Window) Eval(js string) {
	w.do(func() {
		c := C.CString(js)
		C.eval_js(c, 0)
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
		C.eval_js(c, C.gint64(id))
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
	if !w.do(func() { got <- C.GoString(C.page_title()) }) {
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
		C.set_title(c)
		C.free(unsafe.Pointer(c))
	})
}

// Show brings the window to the front.
func (w *Window) Show() { w.do(func() { C.present() }) }

// Close closes the window (as if the user did) and waits until it's gone.
func (w *Window) Close() {
	if w.do(func() { C.destroy_window() }) {
		<-w.closed
	}
}
