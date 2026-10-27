package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Asking is on by default, also for settings saved before the option existed,
// and stays off once turned off.
func TestAskSaveLocationDefault(t *testing.T) {
	for _, tc := range []struct {
		saved string
		want  bool
	}{
		{"", true}, // a new install
		{`{"downloadDir": "D:\\dl", "scanDownloads": true}`, true}, // an older version's settings
		{`{"askSaveLocation": false}`, false},
	} {
		a := &App{configDir: t.TempDir()}
		if tc.saved != "" {
			os.WriteFile(filepath.Join(a.configDir, "settings.json"), []byte(tc.saved), 0o600)
		}
		a.loadSettings()
		if a.settings.AskSaveLocation != tc.want {
			t.Errorf("%q: AskSaveLocation %v, want %v", tc.saved, a.settings.AskSaveLocation, tc.want)
		}
	}
}

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

func TestLocalHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "app.localhost": true, "127.0.0.1": true, "::1": true,
		"192.168.4.26": true, "10.0.0.5": true, "172.16.1.1": true, "fe80::1": true,
		"dl-cdn.alpinelinux.org": false, "1.1.1.1": false, "2606:4700::1111": false, "172.32.0.1": false,
	} {
		if got := localHost(host); got != want {
			t.Errorf("localHost(%q) = %v, want %v", host, got, want)
		}
	}
}
