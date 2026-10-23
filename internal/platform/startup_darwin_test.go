package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
