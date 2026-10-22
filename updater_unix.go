//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// On macOS and with the Linux tarball the app updates itself in place
// (InstallUpdate: canSelfUpdate and applyUpdate in updater_darwin.go and
// updater_linux.go). The pacman package is the package manager's to update,
// so there the app downloads and verifies the package instead
// (DownloadUpdate), as it does wherever it can't write to its own folder.

// selfUpdateAsset is the release file InstallUpdate installs: the one the
// download would fetch.
func selfUpdateAsset(version, exe string) string { return updateAsset(version, exe) }

// canReplace reports whether this user may replace path (in dir): dir is
// writable, and neither it nor a folder above it is writable by everyone
// (/tmp: someone else could swap in their own program between our checks
// and the rename, or later at that path).
func canReplace(dir string) bool {
	return syscall.Access(dir, 2 /* W_OK */) == nil && !worldWritable(dir)
}

// worldWritable reports whether dir or a folder above it can be written by
// everyone. A var so tests can use t.TempDir, which is under /tmp on Linux.
var worldWritable = func(dir string) bool {
	for d := dir; ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm()&0o002 != 0 {
			return true
		}
		if d == filepath.Dir(d) {
			return false
		}
	}
}

// relaunchAfterExit starts cmd once this process has exited, from a small
// shell of its own (the single-instance lock would send a copy started
// sooner straight back to this one). var for tests.
var relaunchAfterExit = func(cmd ...string) error {
	script := `while kill -0 "$1" 2>/dev/null; do sleep 0.3; done; shift; exec "$@"`
	c := exec.Command("/bin/sh", append([]string{"-c", script, "sh", strconv.Itoa(os.Getpid())}, cmd...)...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // not stopped with the app
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}

// updateAsset names the release file for this computer, "" if there's none:
// the pacman package if the app was installed with it, else the tarball.
func updateAsset(version, exe string) string {
	switch {
	case runtime.GOOS == "darwin":
		return "SharkBiteTorrent-macos-universal.zip"
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		if exe == "/usr/bin/sharkbitetorrent" {
			return "sharkbitetorrent-" + strings.ReplaceAll(version, "-", "") + "-1-x86_64.pkg.tar.zst" // see PKGBUILD
		}
		return "SharkBiteTorrent-" + version + "-linux-x86_64.tar.gz"
	}
	return ""
}

func installHint(path string) string {
	switch {
	case strings.HasSuffix(path, ".pkg.tar.zst"):
		// Single-quoted for pasting into a shell, ' itself escaped.
		return "Install it with: sudo pacman -U '" + strings.ReplaceAll(path, "'", `'\''`) + "'"
	case strings.HasSuffix(path, ".zip"):
		return "Open it and drag Shark BiteTorrent into Applications, replacing the old copy."
	}
	return "Extract it and replace your SharkBiteTorrent file with the new one."
}
