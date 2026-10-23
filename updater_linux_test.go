package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The file offered matches how the app was installed.
func TestUpdateAssetLinux(t *testing.T) {
	if got := updateAsset("0.2.37", "/usr/bin/sharkbitetorrent"); got != "sharkbitetorrent-0.2.37-1-x86_64.pkg.tar.zst" {
		t.Errorf("pacman install: %q", got)
	}
	if got := updateAsset("0.2.37", "/home/a/apps/SharkBiteTorrent"); got != "SharkBiteTorrent-0.2.37-linux-x86_64.tar.gz" {
		t.Errorf("tarball: %q", got)
	}
}

func TestFreeNameAndHint(t *testing.T) {
	dir := t.TempDir()
	name := "sharkbitetorrent-0.2.37-1-x86_64.pkg.tar.zst"
	if got := freeName(dir, name); got != filepath.Join(dir, name) {
		t.Errorf("free: %q", got)
	}
	os.WriteFile(filepath.Join(dir, name), nil, 0o644)
	if got := freeName(dir, name); got != filepath.Join(dir, "sharkbitetorrent-0.2.37-1-x86_64 (2).pkg.tar.zst") {
		t.Errorf("taken: %q", got)
	}
	if got := installCommand("/home/o'brien/Downloads/x.pkg.tar.zst"); got != `sudo pacman -U '/home/o'\''brien/Downloads/x.pkg.tar.zst'` {
		t.Errorf("command: %s", got)
	}
	if got := installCommand("/home/o/Downloads/x.tar.gz"); got != "" {
		t.Errorf("tarball command: %q", got)
	}
}

// writeTarball makes a release-style tarball with the given files.
func writeTarball(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "SharkBiteTorrent-0.2.39-linux-x86_64.tar.gz")
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	f.Close()
	return p
}

// The tarball's program (and licenses that are there) replace the running
// copy's; files outside its one folder, or deeper in it, are ignored.
func TestApplyUpdateLinux(t *testing.T) {
	var relaunched []string
	old := relaunchAfterExit
	t.Cleanup(func() { relaunchAfterExit = old })
	relaunchAfterExit = func(cmd ...string) error { relaunched = cmd; return nil }
	ww := worldWritable
	t.Cleanup(func() { worldWritable = ww })
	worldWritable = func(string) bool { return false } // t.TempDir is under /tmp

	dir := t.TempDir()
	exe := filepath.Join(dir, "SharkBiteTorrent")
	os.WriteFile(exe, []byte("old"), 0o755)
	os.WriteFile(filepath.Join(dir, "LICENSE"), []byte("old license"), 0o644)
	if !canSelfUpdate(exe) || canSelfUpdate(packagedExe) {
		t.Fatal("canSelfUpdate: the tarball copy can, the package can't")
	}
	top := "SharkBiteTorrent-0.2.39-linux-x86_64/"
	tb := writeTarball(t, map[string]string{
		top + "SharkBiteTorrent":         "new",
		top + "LICENSE":                  "new license",
		top + "THIRD_PARTY_LICENSES.txt": "not here before: not added",
		top + "x/SharkBiteTorrent":       "deeper: ignored",
		"../SharkBiteTorrent":            "outside: ignored",
		"SharkBiteTorrent":               "not in the folder: ignored",
		"other/SharkBiteTorrent":         "another folder: ignored",
	})
	if err := applyUpdate(tb, "0.2.39", exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Errorf("program: %q", b)
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm() != 0o755 {
		t.Errorf("program mode %v", fi.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "LICENSE")); string(b) != "new license" {
		t.Errorf("license: %q", b)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	hidden, _ := filepath.Glob(filepath.Join(dir, ".*"))
	if len(left) != 2 || len(hidden) != 0 {
		t.Errorf("files left: %v %v", left, hidden)
	}
	if len(relaunched) != 1 || relaunched[0] != exe {
		t.Errorf("relaunch: %v", relaunched)
	}

	os.WriteFile(exe, []byte("old"), 0o755)
	relaunched = nil
	if err := applyUpdate(writeTarball(t, map[string]string{top + "README": "x"}), "0.2.39", exe); err == nil || relaunched != nil {
		t.Error("a tarball without the program was accepted")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Error("the program was touched by a failed update")
	}
}

// Not from a folder anyone can write to (another user could swap in their
// own program at that path).
func TestCanSelfUpdateLinuxWorldWritable(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o777)
	if canSelfUpdate(filepath.Join(dir, "SharkBiteTorrent")) {
		t.Error("a copy in a world-writable folder may update itself")
	}
}

// Temporary files an update killed partway left go at the next start, once
// they're over an hour old.
func TestCleanUpdateLeftoversLinux(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "SharkBiteTorrent")
	os.WriteFile(exe, nil, 0o755)
	stale := filepath.Join(dir, ".SharkBiteTorrent.update-123")
	fresh := filepath.Join(dir, ".LICENSE.update-456")
	os.WriteFile(stale, nil, 0o600)
	os.WriteFile(fresh, nil, 0o600)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(stale, old, old)
	cleanUpdateLeftovers(exe)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale update file was left")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recent one (an update in progress) was removed")
	}
	if _, err := os.Stat(exe); err != nil {
		t.Error("the program was removed")
	}
}
