//go:build windows

package main

import (
	"strings"

	"github.com/wailsapp/go-webview2/pkg/edge"
)

// The app's window is drawn by Microsoft's WebView2 (Edge), which runs in its
// own processes and, left alone, makes its own background requests over the
// normal connection, outside the VPN: proxy auto-discovery (a "wpad" DNS
// lookup) and calls to edge.microsoft.com (configuration, component updates).
// The UI only loads the app's built-in pages, so none of that is needed. The
// release leak test (cmd/leakcheck -app) caught it.
//
// Wails can't pass WebView2 arguments, so this uses a small patch to
// go-webview2 (third_party/go-webview2/PATCHED.md). Wails passes its own
// --disable-features=msSmartScreenProtection first; ours is appended after it
// and Chromium uses only the last --disable-features, so ours repeats it.
// (Don't set Wails' EnableFraudulentWebsiteDetection to avoid that: on macOS
// it turns on Safe Browsing, which talks to Apple/Google outside the VPN.)
var webviewArgs = append([]string{
	// The window never needs the network: its pages are served in-process,
	// links open in the user's browser, and all network work is the app's
	// own Go code (through the VPN). So WebView2 gets no network at all,
	// whatever feature asks for it (Microsoft Editor reached
	// substrate.office.com even with the feature switches below): every
	// hostname except the app's own fails to resolve, and every connection
	// goes to a proxy address where nothing listens. An explicit proxy also
	// turns off proxy auto-discovery (WPAD).
	`--host-resolver-rules="MAP * ~NOTFOUND , EXCLUDE localhost , EXCLUDE wails.localhost"`,
	"--proxy-server=127.0.0.1:9",
}, webviewQuietArgs...)

// webviewQuietArgs turn off WebView2's own background calls to Microsoft.
// The file-host browser window (hostpages_windows.go) uses them too: it does
// reach the network, but only through the VPN, and only for the pages it's sent to.
var webviewQuietArgs = []string{
	// Belt and braces: the individual background features, off.
	"--disable-background-networking", // component updates, field trials, etc.
	"--disable-component-update",
	"--disable-domain-reliability",
	"--disable-client-side-phishing-detection",
	"--disable-sync",
	"--no-pings",
	"--metrics-recording-only", // never upload metrics
	"--disable-breakpad",
	// Features that contact Microsoft from WebView2's browser process, which
	// bypasses the network rules above. Names are taken from the WebView2
	// runtime itself (msedge.dll): the Windows-account lookup and implicit
	// sign-in (substrate.office.com, at startup), text prediction and
	// Microsoft Editor (typed text), and SmartScreen.
	"--disable-features=" + strings.Join([]string{
		"msSmartScreenProtection",
		"msImplicitSignin", "msImplicitSignInNetworkRetry",
		"msEdgeOSAccountInfoSubstrate", "msEdgeOSAccountInfoManagerCache",
		"msOneAuthWAM", "msOneAuthIDHRD", "msSsoByHttpDefer",
		"TextPrediction", "EditorProofing",
		"msEdgeEditorEnvironmentEndpointDetection", "msEdgeEditorSpellcheckerDlpCheck",
		"AutofillServerCommunication", "OptimizationHints", "MediaRouter", "Translate", "msEdgeTranslate",
		// WebRTC's mDNS names for this computer: announced on the home
		// network, outside the VPN. The browser window also removes WebRTC
		// from its pages (internal/browser noRTCScript); this covers a page
		// that gets it back from a fresh frame.
		"WebRtcHideLocalIpsWithMdns",
	}, ","),
}

func init() { edge.ExtraBrowserArgs = webviewArgs }
