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
