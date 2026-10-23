package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"sharkbite/internal/systool"
)

// Linux follows the freedesktop.org standards that KDE (CachyOS default),
// GNOME and others implement: a .desktop entry declaring the MIME types, made
// default with xdg-mime. Distro packages ship their own .desktop file in
// /usr/share/applications; this per-user one covers running a bare binary.

const desktopID = "sharkbitetorrent.desktop"

// packagedExe is where the distro package installs the app, with its entry
// in /usr/share/applications.
const packagedExe = "/usr/bin/sharkbitetorrent"

var mimeTypes = []string{"x-scheme-handler/magnet", "application/x-bittorrent"}

func OpenFolder(dir string) error { return systool.Command("xdg-open", dir).Start() }

func desktopPath() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "applications", desktopID)
}

// desktopExec quotes exe for a .desktop Exec= line. Paths with characters
// that format treats specially are refused rather than escaped.
func desktopExec(exe string) (string, error) {
	if strings.ContainsAny(exe, "\"`$\\%") || strings.IndexFunc(exe, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("the app's path %q has characters a desktop entry can't hold (\" ` $ \\ %%); move the app to another folder", exe)
	}
	return `"` + exe + `"`, nil
}

func RegisterAssociations(exe string) error {
	if exe == packagedExe {
		// The package's entry has the same name: a per-user one (written by
		// a copy run from elsewhere) would hide it, so magnet links and the
		// menu would open that copy.
		dropUserEntry()
		return setDefaults()
	}
	if err := writeEntry(exe); err != nil {
		return err
	}
	return setDefaults()
}

// writeEntry writes the per-user .desktop entry for exe.
func writeEntry(exe string) error {
	execPath, err := desktopExec(exe)
	if err != nil {
		return err
	}
	entry := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Shark BiteTorrent
Comment=BitTorrent client with a built-in VPN
Exec=%s %%u
Icon=sharkbitetorrent
Terminal=false
Categories=Network;FileTransfer;P2P;
MimeType=%s;
StartupWMClass=sharkbitetorrent
`, execPath, strings.Join(mimeTypes, ";"))
	p := desktopPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(entry), 0o644); err != nil {
		return err
	}
	systool.Command("update-desktop-database", filepath.Dir(p)).Run() // optional helper
	return nil
}

func setDefaults() error {
	for _, m := range mimeTypes {
		if out, err := systool.Command("xdg-mime", "default", desktopID, m).CombinedOutput(); err != nil {
			return fmt.Errorf("xdg-mime default %s: %v: %s", m, err, out)
		}
	}
	return nil
}

func dropUserEntry() {
	if os.Remove(desktopPath()) == nil {
		systool.Command("update-desktop-database", filepath.Dir(desktopPath())).Run()
	}
}

// RefreshAssociations runs at startup. The packaged app removes a per-user
// entry left by a copy run from elsewhere (a downloaded tarball, before
// installing the package), which hides the package's entry. A copy run
// from elsewhere that was moved points its entry at where it is now. As
// with sign-in start, only when the copy the entry names is gone (another
// copy still there keeps it), and never to a copy in a folder any user can
// write to (/tmp): someone else could put a program at that path later.
func RefreshAssociations(exe string) {
	b, err := os.ReadFile(desktopPath())
	if err != nil {
		return
	}
	if exe == packagedExe {
		dropUserEntry()
		return
	}
	m := execLine.FindSubmatch(b)
	if m == nil || string(m[1]) == exe || inWorldWritableDir(exe) {
		return
	}
	if _, err := os.Stat(string(m[1])); err != nil {
		writeEntry(exe) // not the defaults: another app may have been chosen since
	}
}

func UnregisterAssociations(string) error {
	err := os.Remove(desktopPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func isDefault(mime string) bool {
	out, err := systool.Command("xdg-mime", "query", "default", mime).Output()
	return err == nil && strings.TrimSpace(string(out)) == desktopID
}

func Associations(string) AssocStatus {
	return AssocStatus{
		Supported: true,
		Magnet:    isDefault("x-scheme-handler/magnet"),
		Torrent:   isDefault("application/x-bittorrent"),
	}
}

func OpenDefaultAppsSettings() error {
	return errors.New("use your desktop's Default Applications settings")
}

// RevealPath opens the folder (or the folder containing the file). There's
// no portable "select this file" across Linux file managers.
func RevealPath(path string, isDir bool) error {
	if !isDir {
		path = filepath.Dir(path)
	}
	return systool.Command("xdg-open", path).Start()
}

// OpenURL opens a web link in the default browser (xdg-open from a fixed
// system folder, not PATH).
func OpenURL(link string) error { return systool.Command("xdg-open", link).Start() }
