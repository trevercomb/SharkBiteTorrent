//go:build (linux || darwin) && cgo

package main

// webviewQuietArgs are Chromium switches for WebView2 (webview_windows.go);
// WebKit has none: its windows turn the equivalent off in their settings
// (internal/browser/window_linux.go, window_darwin.go).
var webviewQuietArgs []string
