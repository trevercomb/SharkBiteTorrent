//go:build windows

package browser

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as a fake window process (no real window): it
// speaks the protocol, and "navigate crash" makes it die like a WebView2
// failure would.
func TestMain(m *testing.M) {
	if os.Getenv("SBT_FAKE_BROWSER_WINDOW") == "1" {
		fakeHelper()
		return
	}
	os.Exit(m.Run())
}

func fakeHelper() {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	var cfg helperConfig
	if dec.Decode(&cfg) != nil || cfg.Proxy == "" {
		enc.Encode(event{Ev: "error", Text: "bad configuration"})
		os.Exit(1)
	}
	enc.Encode(event{Ev: "ready"})
	for {
		var c command
		if dec.Decode(&c) != nil {
			os.Exit(0) // the app went away
		}
		switch c.Cmd {
		case "navigate":
			if c.Arg == "crash" {
				os.Exit(3)
			}
			enc.Encode(event{Ev: "loaded", URL: c.Arg})
			enc.Encode(event{Ev: "download", URL: c.Arg + "/file.iso", Disposition: `attachment; filename="file.iso"`})
		case "title":
			enc.Encode(event{Ev: "title", ID: c.ID, Text: "Page title"})
		case "close":
			os.Exit(0)
		}
	}
}

func startFake(t *testing.T, o Options) *Remote {
	t.Helper()
	t.Setenv("SBT_FAKE_BROWSER_WINDOW", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, err := Start(exe, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRemote(t *testing.T) {
	var mu sync.Mutex
	var loaded []string
	var downloads []Download
	closed := make(chan struct{})
	r := startFake(t, Options{
		Proxy: "127.0.0.1:1", DataDir: t.TempDir(),
		OnLoaded: func(u string) { mu.Lock(); loaded = append(loaded, u); mu.Unlock() },
		OnDownload: func(d Download) DownloadChoice {
			mu.Lock()
			downloads = append(downloads, d)
			mu.Unlock()
			return DownloadChoice{}
		},
		OnClosed: func() { close(closed) },
	})
	r.Navigate("https://host.example/f/1")
	if got := r.DocumentTitle(); got != "Page title" {
		t.Errorf("title %q", got)
	}
	mu.Lock()
	if len(loaded) != 1 || len(downloads) != 1 || downloads[0].URL != "https://host.example/f/1/file.iso" {
		t.Errorf("loaded %v, downloads %+v", loaded, downloads)
	}
	mu.Unlock()
	r.Close()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("OnClosed not called")
	}
}

// If the window's process dies (WebView2 failed), the app just sees the
// window gone; it doesn't hang or crash.
func TestRemoteCrash(t *testing.T) {
	r := startFake(t, Options{Proxy: "127.0.0.1:1", DataDir: t.TempDir()})
	r.Navigate("crash")
	select {
	case <-r.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done not closed after the window's process died")
	}
	if got := r.DocumentTitle(); got != "" {
		t.Errorf("title from a dead window: %q", got)
	}
	r.Navigate("https://host.example/") // ignored, no panic
	r.Close()                           // returns at once
}

func TestRemoteBadStart(t *testing.T) {
	t.Setenv("SBT_FAKE_BROWSER_WINDOW", "1")
	exe, _ := os.Executable()
	if _, err := Start(exe, Options{DataDir: t.TempDir()}); err == nil { // no proxy: the fake refuses
		t.Fatal("started without a proxy")
	}
}
