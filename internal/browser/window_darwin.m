// The macOS browser window's Cocoa side (window_darwin.go).

#include <stdlib.h>
#include <stdint.h>
#include <pthread.h>
#include <objc/message.h>
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <Network/Network.h>
#include "window_darwin.h"

static NSWindow *win;
static WKWebView *view;
static NSMutableSet *standins; // pop-ups' stand-ins, until they're dropped

static void run_queue(void *d) { goRunQueue(); }
void sbw_wakeup(void) { dispatch_async_f(dispatch_get_main_queue(), NULL, run_queue); }

static char *cstr(NSString *s) { return (char *)(s ? s.UTF8String : ""); }

// Every navigation in a pop-up's stand-in is refused (nothing loads).
@interface SBDenyAll : NSObject <WKNavigationDelegate>
@end
@implementation SBDenyAll
- (void)webView:(WKWebView *)v decidePolicyForNavigationAction:(WKNavigationAction *)a decisionHandler:(void (^)(WKNavigationActionPolicy))h {
	h(WKNavigationActionPolicyCancel);
}
@end
static SBDenyAll *denyAll;

@interface SBBrowserDelegate : NSObject <NSWindowDelegate, WKNavigationDelegate, WKUIDelegate, WKDownloadDelegate>
@end
@implementation SBBrowserDelegate
- (void)windowWillClose:(NSNotification *)n {
	view = nil;
	win = nil;
	[NSApp stop:nil];
	// stop: takes effect after an event; post one so run returns now.
	[NSApp postEvent:[NSEvent otherEventWithType:NSEventTypeApplicationDefined location:NSZeroPoint
		modifierFlags:0 timestamp:0 windowNumber:0 context:nil subtype:0 data1:0 data2:0] atStart:YES];
}

- (void)webView:(WKWebView *)v didFinishNavigation:(WKNavigation *)nav {
	goLoaded(cstr(v.URL.absoluteString));
}

// <a download> (and other downloads WebKit decides on before a response).
- (void)webView:(WKWebView *)v decidePolicyForNavigationAction:(WKNavigationAction *)a decisionHandler:(void (^)(WKNavigationActionPolicy))h {
	if (a.shouldPerformDownload) {
		if (goDownload(cstr(a.request.URL.absoluteString), "", "", -1)) h(WKNavigationActionPolicyCancel);
		else h(WKNavigationActionPolicyDownload);
		return;
	}
	h(WKNavigationActionPolicyAllow);
}

// A response it can't show, or one sent as an attachment, is a download,
// from the page or any frame in it, as on Windows and Linux.
- (void)webView:(WKWebView *)v decidePolicyForNavigationResponse:(WKNavigationResponse *)r decisionHandler:(void (^)(WKNavigationResponsePolicy))h {
	NSString *cd = nil;
	if ([r.response isKindOfClass:[NSHTTPURLResponse class]])
		cd = [(NSHTTPURLResponse *)r.response valueForHTTPHeaderField:@"Content-Disposition"];
	BOOL attachment = cd && [cd.lowercaseString hasPrefix:@"attachment"];
	if (!attachment && r.canShowMIMEType) {
		h(WKNavigationResponsePolicyAllow);
		return;
	}
	if (goDownload(cstr(r.response.URL.absoluteString), cstr(cd), cstr(r.response.MIMEType), r.response.expectedContentLength))
		h(WKNavigationResponsePolicyCancel); // the app downloads it itself
	else
		h(WKNavigationResponsePolicyDownload);
}

// Downloads the app lets the window make itself go to ~/Downloads.
- (void)webView:(WKWebView *)v navigationAction:(WKNavigationAction *)a didBecomeDownload:(WKDownload *)d { d.delegate = self; }
- (void)webView:(WKWebView *)v navigationResponse:(WKNavigationResponse *)r didBecomeDownload:(WKDownload *)d { d.delegate = self; }
- (void)download:(WKDownload *)d decideDestinationUsingResponse:(NSURLResponse *)r suggestedFilename:(NSString *)name completionHandler:(void (^)(NSURL *))h {
	NSURL *dir = [NSFileManager.defaultManager URLsForDirectory:NSDownloadsDirectory inDomains:NSUserDomainMask].firstObject;
	h([dir URLByAppendingPathComponent:name.lastPathComponent]);
}

// A pop-up the page asked for: reported, and nothing opens. The page gets a
// hidden stand-in rather than none, as on Windows and Linux: an ad script
// that hijacks the download button's first clicks to open an ad lets the
// next click through once it believes its ad opened.
- (WKWebView *)webView:(WKWebView *)v createWebViewWithConfiguration:(WKWebViewConfiguration *)c forNavigationAction:(WKNavigationAction *)a windowFeatures:(WKWindowFeatures *)f {
	goPopup(cstr(a.request.URL.absoluteString), a.navigationType == WKNavigationTypeLinkActivated);
	WKWebView *standin = [[WKWebView alloc] initWithFrame:NSZeroRect configuration:c];
	standin.navigationDelegate = denyAll;
	[standins addObject:standin];
	dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 60 * NSEC_PER_SEC), dispatch_get_main_queue(), ^{ [standins removeObject:standin]; });
	return standin;
}

// No camera or microphone (nothing that could connect around the proxy).
- (void)webView:(WKWebView *)v requestMediaCapturePermissionForOrigin:(WKSecurityOrigin *)o initiatedByFrame:(WKFrameInfo *)f type:(WKMediaCaptureType)t decisionHandler:(void (^)(WKPermissionDecision))h {
	h(WKPermissionDecisionDeny);
}
@end
static SBBrowserDelegate *delegate;

// 0: open; 1: macOS is older than 14; 2: not on the main thread.
int sbw_open_window(const char *proxy, const char *user, const char *pass, const char *store, const char *title) {
	if (!pthread_main_np()) return 2;
	if (@available(macOS 14.0, *)) {
		[NSApplication sharedApplication];
		// No Dock icon of its own: the app's is already there.
		[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
		NSMenu *bar = [NSMenu new], *edit = [[NSMenu alloc] initWithTitle:@"Edit"];
		[edit addItemWithTitle:@"Cut" action:@selector(cut:) keyEquivalent:@"x"];
		[edit addItemWithTitle:@"Copy" action:@selector(copy:) keyEquivalent:@"c"];
		[edit addItemWithTitle:@"Paste" action:@selector(paste:) keyEquivalent:@"v"];
		[edit addItemWithTitle:@"Select All" action:@selector(selectAll:) keyEquivalent:@"a"];
		[edit addItemWithTitle:@"Close Window" action:@selector(performClose:) keyEquivalent:@"w"];
		[[bar addItemWithTitle:@"Edit" action:nil keyEquivalent:@""] setSubmenu:edit];
		NSApp.mainMenu = bar;

		delegate = [SBBrowserDelegate new];
		denyAll = [SBDenyAll new];
		standins = [NSMutableSet new];

		// Its own cookies and cache, kept between runs.
		WKWebsiteDataStore *ds = [WKWebsiteDataStore dataStoreForIdentifier:[[NSUUID alloc] initWithUUIDString:@(store)]];
		// Everything through the app's SOCKS5 proxy, which looks host names
		// up inside the VPN; it has a password, which only this window has.
		NSArray *hp = [@(proxy) componentsSeparatedByString:@":"];
		nw_endpoint_t ep = nw_endpoint_create_host([hp[0] UTF8String], [hp[1] UTF8String]);
		nw_proxy_config_t pc = nw_proxy_config_create_socksv5(ep);
		nw_proxy_config_set_username_and_password(pc, user, pass);
		ds.proxyConfigurations = @[pc];

		WKWebViewConfiguration *c = [WKWebViewConfiguration new];
		c.websiteDataStore = ds;
		// Safari's own name: the same WebKit, and pages that look for
		// "Safari" otherwise take it for something that isn't a browser.
		NSInteger major = NSProcessInfo.processInfo.operatingSystemVersion.majorVersion;
		c.applicationNameForUserAgent = [NSString stringWithFormat:@"Version/%ld.0 Safari/605.1.15", (long)(major >= 26 ? major : major + 3)];
		// Pop-ups reach createWebView (which never opens one): refused
		// outright, an ad script keeps retrying on every click.
		c.preferences.javaScriptCanOpenWindowsAutomatically = YES;
		// No WebRTC: its connections (STUN, peer to peer) don't go through
		// the proxy. WebKit has no public switch; this one is its own.
		SEL noRTC = NSSelectorFromString(@"_setPeerConnectionEnabled:");
		if ([c.preferences respondsToSelector:noRTC])
			((void (*)(id, SEL, BOOL))objc_msgSend)(c.preferences, noRTC, NO);

		NSRect frame = NSMakeRect(0, 0, 1100, 760);
		view = [[WKWebView alloc] initWithFrame:frame configuration:c];
		view.navigationDelegate = delegate;
		view.UIDelegate = delegate;
		win = [[NSWindow alloc] initWithContentRect:frame
			styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable | NSWindowStyleMaskMiniaturizable | NSWindowStyleMaskResizable
			backing:NSBackingStoreBuffered defer:NO];
		win.releasedWhenClosed = NO;
		win.title = @(title);
		win.contentView = view;
		win.delegate = delegate;
		[win center];
		[win makeKeyAndOrderFront:nil];
		[NSApp activateIgnoringOtherApps:YES];
		return 0;
	}
	return 1;
}

// run_app runs the window until it closes.
void sbw_run_app(void) { [NSApp run]; }

void sbw_eval_js(const char *js, int64_t qid) {
	if (!view) {
		if (qid) goResult(qid, "");
		return;
	}
	[view evaluateJavaScript:@(js) completionHandler:^(id v, NSError *err) {
		if (!qid) return;
		if (err) {
			// undefined (or a function, say) has no JSON: null, as on Linux.
			goResult(qid, err.code == WKErrorJavaScriptResultTypeIsUnsupported ? "null" : "");
			return;
		}
		if (!v) {
			goResult(qid, "null");
			return;
		}
		NSData *d = [NSJSONSerialization dataWithJSONObject:v options:NSJSONWritingFragmentsAllowed | NSJSONWritingWithoutEscapingSlashes error:nil];
		NSString *s = d ? [[NSString alloc] initWithData:d encoding:NSUTF8StringEncoding] : nil;
		goResult(qid, s ? cstr(s) : "null");
	}];
}

void sbw_load_uri(const char *u) {
	NSURL *url = [NSURL URLWithString:@(u)];
	if (view && url) [view loadRequest:[NSURLRequest requestWithURL:url]];
}
const char *sbw_page_title(void) { return view ? cstr(view.title) : ""; }
void sbw_set_title(const char *t) { if (win) win.title = @(t); }
void sbw_present(void) {
	if (!win) return;
	[win makeKeyAndOrderFront:nil];
	[NSApp activateIgnoringOtherApps:YES];
}
void sbw_close_window(void) { if (win) [win close]; }
