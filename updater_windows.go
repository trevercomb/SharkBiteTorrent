package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

// uninstKey is where the installer registers itself (wails_tools.nsh:
// INFO_COMPANYNAME + INFO_PRODUCTNAME).
const uninstKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\SharkBiteTorrentShark BiteTorrent`

// canSelfUpdate is true for the installed version: the folder the installer
// recorded, or (installs from before it recorded one) a folder with its
// uninstall.exe. The portable exe can't replace itself while running, so it
// gets a download link instead.
func canSelfUpdate(exe string) bool {
	dir := filepath.Dir(exe)
	if k, err := registry.OpenKey(registry.CURRENT_USER, uninstKey, registry.QUERY_VALUE); err == nil {
		loc, _, err := k.GetStringValue("InstallLocation")
		k.Close()
		if err == nil && loc != "" {
			return strings.EqualFold(filepath.Clean(loc), filepath.Clean(dir))
		}
	}
	_, err := os.Stat(filepath.Join(dir, "uninstall.exe"))
	return err == nil
}

// runInstaller starts the downloaded installer silently, installing into the
// running app's folder. It closes the app, upgrades in place and relaunches
// it. NSIS wants /D= last and unquoted even with spaces, so the command line
// is written out as-is instead of letting Go quote it.
func runInstaller(path string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + path + `" /S /D=` + filepath.Dir(exe)}
	return cmd.Start()
}

// selfUpdateAsset is the installer InstallUpdate runs.
func selfUpdateAsset(string, string) string { return installerAsset }

// applyUpdate runs the downloaded installer (checked against the release's
// signed checksums), which closes the app, upgrades it and relaunches it.
func applyUpdate(path, _, _ string) error { return runInstaller(path) }

// cleanUpdateLeftovers: the installer cleans up after itself (and its
// folder goes with the sharkbite-update-* ones cleanUpdateTemp removes).
func cleanUpdateLeftovers(string) {}

// Windows installs updates itself (InstallUpdate).
func updateAsset(string, string) string { return "" }

func installHint(string) string { return "" }

func installCommand(string) string { return "" }
