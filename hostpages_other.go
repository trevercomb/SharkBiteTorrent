//go:build !windows && !(linux && cgo)

package main

import "errors"

// The file-host browser window needs Windows or Linux with WebKitGTK (hostpages.go).

type hostPage struct {
	URL    string
	ID     string
	Dir    string // where its download goes ("" = the default folder)
	Paused bool   // add the download paused
}

type hostBrowser struct{}

func (a *App) openHostPages([]hostPage) error {
	return errors.New("links to file hosts' pages need the browser window, which isn't available on this system; direct file links and torrents work")
}

func (a *App) closeHostPages() {}

// The browser window's process exists on Windows only.
const browserWindowArg = "--browser-window"

func runBrowserWindow() error { return errors.New("the browser window is Windows-only") }
