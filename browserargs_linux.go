//go:build linux && cgo

package main

// webviewQuietArgs are Chromium switches for WebView2 (webview_windows.go);
// WebKitGTK has none: its window turns the equivalent off in its settings
// (internal/browser/window_linux.go).
var webviewQuietArgs []string
