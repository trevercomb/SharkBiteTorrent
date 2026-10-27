package main

import (
	"os"
	"testing"
	"time"
)

// Windows reuses process IDs: an app's process that exited must not make a
// later, unrelated process with the same ID count as the app's.
func TestProcessTreeReusedID(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	const root, webview, gpu, other = 100, 200, 300, 999
	set := processTree(root, "sharkbitetorrent.exe", []procEvent{
		{at: at(1), pid: webview, parent: root, image: "msedgewebview2.exe"},
		{at: at(2), pid: gpu, parent: webview, image: "msedgewebview2.exe"},
		{at: at(10), pid: gpu, exit: true},
		{at: at(11), pid: webview, exit: true},
		// ID 200 reused by another program, started by something else.
		{at: at(20), pid: webview, parent: other, image: "nvcontainer.exe"},
		// A child of the reused ID isn't the app's either.
		{at: at(21), pid: 400, parent: webview, image: "nvcontainer.exe"},
	})
	for _, c := range []struct {
		pid   int
		at    int
		image string
		want  bool
	}{
		{root, 50, "sharkbitetorrent.exe", true},
		{webview, 5, "msedgewebview2.exe", true},
		{webview, 5, "", true}, // a DNS lookup names no program
		{webview, 25, "nvcontainer.exe", false},
		{webview, 25, "", false},
		{webview, 5, "nvcontainer.exe", false}, // right time, wrong program
		{gpu, 9, "msedgewebview2.exe", true},
		{gpu, 15, "msedgewebview2.exe", false},
		{400, 25, "nvcontainer.exe", false},
	} {
		if got := set.hasAt(c.pid, at(c.at), c.image); got != c.want {
			t.Errorf("pid %d at +%ds (%q): %v, want %v", c.pid, c.at, c.image, got, c.want)
		}
	}
}

// The polling backup (addChildren) ends a process's span when another
// program shows up under its ID.
func TestSeenReused(t *testing.T) {
	t0 := time.Now().UTC()
	s := &pidSet{pids: map[int]bool{7: true}, spans: map[int][]span{}}
	s.seenLocked(7, "msedgewebview2.exe", t0, t0.Add(-2*time.Second)) // started since the poll before
	s.seenLocked(7, "msedgewebview2.exe", t0.Add(5*time.Second), t0)
	s.seenLocked(7, "", t0.Add(6*time.Second), t0)                // exited
	s.seenLocked(7, "nvcontainer.exe", t0.Add(7*time.Second), t0) // reused
	if !s.reusedLocked(7, "nvcontainer.exe") {
		t.Error("reuse not noticed")
	}
	if !s.hasAt(7, t0.Add(-2*time.Second), "") || !s.hasAt(7, t0.Add(3*time.Second), "") || s.hasAt(7, t0.Add(30*time.Second), "") {
		t.Errorf("spans: %+v", s.spans[7])
	}
}

// A set's root (the check itself, or the app it launched) counts for the
// whole run, also for events from before the set was made: the check's own
// test connection at the start must be found (a run failed on that).
func TestRootWholeLife(t *testing.T) {
	before := time.Now().UTC().Add(-5 * time.Minute)
	s := newPIDSet(os.Getpid())
	if !s.hasAt(os.Getpid(), before, "") || !s.hasAt(os.Getpid(), time.Now().UTC().Add(time.Hour), "") {
		t.Errorf("root span: %+v", s.spans[os.Getpid()])
	}
	s.addChildren() // polling again doesn't cut it short
	if !s.hasAt(os.Getpid(), before, "") {
		t.Errorf("after polling: %+v", s.spans[os.Getpid()])
	}
	// Merged into a set from the logs, the logs' root span still rules.
	logs := processTree(os.Getpid(), "", nil)
	logs.merge(s)
	if !logs.hasAt(os.Getpid(), before, "") {
		t.Error("merged root lost")
	}
}

func TestCheckBrowserArgs(t *testing.T) {
	quiet := "--disable-features=msSmartScreenProtection,msImplicitSignin,msEdgeOSAccountInfoSubstrate,TextPrediction,WebRtcHideLocalIpsWithMdns"
	main := `msedgewebview2.exe --embedded-browser-webview=1 --host-resolver-rules="MAP * ~NOTFOUND , EXCLUDE localhost" --proxy-server=127.0.0.1:9 ` + quiet
	host := `msedgewebview2.exe --embedded-browser-webview=1 --proxy-server=socks5://127.0.0.1:51234 --host-resolver-rules="MAP * ~NOTFOUND , EXCLUDE 127.0.0.1" --disable-quic --force-webrtc-ip-handling-policy=disable_non_proxied_udp --proxy-bypass-list=<-loopback> ` + quiet
	set := func(cmds ...string) *pidSet {
		s := &pidSet{pids: map[int]bool{}, kind: map[int]string{}, cmd: map[int]string{}}
		for i, c := range cmds {
			s.pids[i], s.kind[i], s.cmd[i] = true, "msedgewebview2.exe", c
		}
		return s
	}
	if msg := checkBrowserArgs(set(main, host)); msg != "" {
		t.Errorf("both windows as built: %s", msg)
	}
	if msg := checkBrowserArgs(set(host)); msg == "" {
		t.Error("only the file-host window seen: the main window's settings went unchecked")
	}
	noQuic := `msedgewebview2.exe --proxy-server=socks5://127.0.0.1:51234 --host-resolver-rules="MAP * ~NOTFOUND , EXCLUDE 127.0.0.1" ` + quiet
	if msg := checkBrowserArgs(set(main, noQuic)); msg == "" {
		t.Error("a file-host window without --disable-quic passed")
	}
	twoProxies := host + " --proxy-server=127.0.0.1:9"
	if msg := checkBrowserArgs(set(main, twoProxies)); msg == "" {
		t.Error("a file-host window with two proxies passed")
	}
}

// The app's own start event (by Explorer, which isn't watched) comes first
// in the log; it must not drop the app, or none of its children would be
// followed (a fresh-eyes audit found that).
func TestProcessTreeRootStartEvent(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	const explorer, root, webview, network = 10, 100, 200, 300
	set := processTree(root, "sharkbitetorrent.exe", []procEvent{
		{at: at(0), pid: root, parent: explorer, image: "sharkbitetorrent.exe"},
		{at: at(1), pid: webview, parent: root, image: "msedgewebview2.exe"},
		{at: at(1), pid: network, parent: webview, image: "msedgewebview2.exe"}, // short-lived
		{at: at(2), pid: network, exit: true},
		{at: at(60), pid: root, exit: true},
		{at: at(61), pid: root, parent: explorer, image: "other.exe"}, // the app's ID reused
	})
	for _, c := range []struct {
		pid   int
		at    int
		image string
		want  bool
	}{
		{root, 30, "sharkbitetorrent.exe", true},
		{webview, 1, "msedgewebview2.exe", true}, // its very first second
		{network, 1, "msedgewebview2.exe", true}, // lived one second, never polled
		{root, 90, "other.exe", false},
		{root, 90, "", false},
	} {
		if got := set.hasAt(c.pid, at(c.at), c.image); got != c.want {
			t.Errorf("pid %d at +%ds (%q): %v, want %v", c.pid, c.at, c.image, got, c.want)
		}
	}
	if !set.has(webview) || !set.has(network) {
		t.Errorf("children not followed: %v", set.pids)
	}
}

// Engine mode: the root is the check itself (leakcheck.exe, or a go run
// build), so its connections count whatever its program is called.
func TestProcessTreeEngineRoot(t *testing.T) {
	set := processTree(4242, "", nil)
	if !set.hasAt(4242, time.Now(), "leakcheck.exe") {
		t.Error("the check's own connections are dropped in engine mode")
	}
}
