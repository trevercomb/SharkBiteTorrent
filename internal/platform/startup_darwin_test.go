package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func autostartFile() string { return agentPath() }

// An update changes the program, and macOS then refuses to start it from
// the old agent: the agent is registered again, once per change, and never
// from the agent's own process.
func TestRefreshStartupReregistersAfterUpdate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reloads := 0
	oldReload, oldSignIn := reloadAgent, startedAtSignIn
	t.Cleanup(func() { reloadAgent, startedAtSignIn = oldReload, oldSignIn })
	reloadAgent = func() { reloads++ }
	signIn := false
	startedAtSignIn = func() bool { return signIn }

	exe := filepath.Join(t.TempDir(), "SharkBiteTorrent")
	os.WriteFile(exe, []byte("version 1"), 0o755)
	if err := SetStartup(exe, true); err != nil {
		t.Fatal(err)
	}
	RefreshStartup(exe)
	if reloads != 0 {
		t.Fatal("re-registered an unchanged program")
	}
	os.WriteFile(exe, []byte("version 2"), 0o755) // updated
	signIn = true
	RefreshStartup(exe)
	if reloads != 0 {
		t.Fatal("re-registered from the agent's own process (unloading it would stop the app)")
	}
	signIn = false
	RefreshStartup(exe)
	if reloads != 1 {
		t.Fatalf("not re-registered after the program changed (%d)", reloads)
	}
	RefreshStartup(exe)
	if reloads != 1 {
		t.Error("re-registered again for the same program")
	}
	b, _ := os.ReadFile(agentPath())
	if !strings.Contains(string(b), fingerprint(exe)) || !strings.Contains(string(b), "<string>"+exe+"</string>") {
		t.Errorf("agent:\n%s", b)
	}
}

// A start doesn't hash the program again while its size and time are the
// ones recorded, and an unreadable program never re-registers.
func TestRefreshStartupStampAndUnreadable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reloads := 0
	oldReload, oldSignIn := reloadAgent, startedAtSignIn
	t.Cleanup(func() { reloadAgent, startedAtSignIn = oldReload, oldSignIn })
	reloadAgent = func() { reloads++ }
	startedAtSignIn = func() bool { return false }

	exe := filepath.Join(t.TempDir(), "SharkBiteTorrent")
	os.WriteFile(exe, []byte("v1"), 0o755)
	SetStartup(exe, true)
	b, _ := os.ReadFile(agentPath())
	if !strings.Contains(string(b), stampKey) || !strings.Contains(string(b), stamp(exe)) {
		t.Fatalf("no stamp:\n%s", b)
	}
	// Same size and time, different bytes: trusted as unchanged (no hash).
	fi, _ := os.Stat(exe)
	os.WriteFile(exe, []byte("v2"), 0o755)
	os.Chtimes(exe, fi.ModTime(), fi.ModTime())
	RefreshStartup(exe)
	if reloads != 0 {
		t.Error("hashed and re-registered although size and time were unchanged")
	}
	// Unreadable now (and the stamp changed): no re-registering.
	os.Chmod(exe, 0o000)
	os.Chtimes(exe, fi.ModTime().Add(time.Second), fi.ModTime().Add(time.Second))
	RefreshStartup(exe)
	os.Chmod(exe, 0o755)
	if reloads != 0 {
		t.Error("re-registered for a program that couldn't be read")
	}
	// Readable and changed: once.
	RefreshStartup(exe)
	RefreshStartup(exe)
	if reloads != 1 {
		t.Errorf("re-registered %d times after a real change, want 1", reloads)
	}
	// A file from an older version, without a stamp, gets one without a
	// reload.
	b, _ = os.ReadFile(agentPath())
	old := agentStamp.ReplaceAll(b, nil)
	old = []byte(strings.Replace(string(old), "<key>"+stampKey+"</key>", "", 1))
	os.WriteFile(agentPath(), old, 0o644)
	RefreshStartup(exe)
	if b, _ := os.ReadFile(agentPath()); reloads != 1 || !strings.Contains(string(b), stamp(exe)) {
		t.Errorf("stamp not added quietly (reloads %d):\n%s", reloads, b)
	}
}
