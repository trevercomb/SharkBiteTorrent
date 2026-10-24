//go:build !windows

package main

import "errors"

// The file-host browser window is Windows-only so far (hostpages_windows.go).

type hostPage struct {
	URL string
	ID  string
}

type hostBrowser struct{}

func (a *App) openHostPages([]hostPage) error {
	return errors.New("links to file hosts' pages need the browser window, which is only on Windows so far; direct file links and torrents work")
}

func (a *App) closeHostPages() {}
