package main

import (
	"testing"

	"sharkbite/internal/ddl"
)

// A direct download's name gets the same disguised-program check as a
// torrent's file list.
func TestDownloadNameWarnings(t *testing.T) {
	a := &App{}
	for name, want := range map[string]bool{
		"Some.Movie.2026.1080p.mp4.exe": true,
		"Some.Movie.2026.1080p.mkv":     false,
		"debian-13.iso":                 false,
	} {
		ds := a.downloadStatus(ddl.Status{Name: name, Size: 700 << 20}, ddl.Spec{})
		if got := len(ds.Warnings) > 0; got != want {
			t.Errorf("%s: warnings %v, want any: %v", name, ds.Warnings, want)
		}
	}
}

// A page's download named like a disguised program is added paused.
func TestNameWarning(t *testing.T) {
	for name, flagged := range map[string]bool{
		"Movie.2026.1080p.mp4.exe": true, "invoice.pdf.scr": true,
		"debian-13.7.0-amd64-netinst.iso": false, "vlc-3.0.24-win64.exe": false, "": false,
	} {
		if got := nameWarning(name) != ""; got != flagged {
			t.Errorf("%q: flagged %v, want %v", name, got, flagged)
		}
	}
}
