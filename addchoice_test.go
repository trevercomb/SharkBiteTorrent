package main

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestRecentDirs(t *testing.T) {
	base := t.TempDir()
	a := &App{configDir: base}
	a.settings.DownloadDir = filepath.Join(base, "nas")
	d := func(n string) string { return filepath.Join(base, n) }

	a.noteRecentDir(a.settings.DownloadDir) // the default isn't "recent"
	if len(a.settings.RecentDirs) != 0 {
		t.Fatalf("default recorded: %v", a.settings.RecentDirs)
	}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "b"} {
		a.noteRecentDir(d(n))
	}
	want := []string{d("b"), d("f"), d("e"), d("d"), d("c")} // newest first, no repeats, at most 5
	if !slices.Equal(a.settings.RecentDirs, want) {
		t.Errorf("recent: %v, want %v", a.settings.RecentDirs, want)
	}

	if got := a.dirFor(AddChoice{}); got != a.settings.DownloadDir {
		t.Errorf("no choice: %s", got)
	}
	if got := a.dirFor(AddChoice{Dir: d("x") + string(filepath.Separator)}); got != d("x") {
		t.Errorf("chosen: %s", got)
	}

	// Saving Settings from the form keeps the list (it's changed as folders are used).
	s := a.settings
	s.RecentDirs = nil
	if err := a.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.settings.RecentDirs, want) {
		t.Errorf("after SaveSettings: %v", a.settings.RecentDirs)
	}
}
