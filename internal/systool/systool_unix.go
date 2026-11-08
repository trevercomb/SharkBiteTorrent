//go:build !windows

package systool

import (
	"os"
	"path/filepath"
)

// The system's own folders come first, so a system tool is always found
// there; the local and Homebrew folders are for tools the user installed
// (ClamAV, Tailscale's CLI). Those are writable by the user on macOS, so a
// program running as the user could replace them; that's no more than it
// could do to the app itself. /usr/local/clamav/bin is where ClamAV's own
// macOS installer (the .pkg from clamav.net) puts it, owned by root.
var dirs = []string{
	"/usr/bin", "/bin", "/usr/sbin", "/sbin",
	"/usr/local/bin", "/usr/local/sbin", "/usr/local/clamav/bin", "/opt/homebrew/bin", "/opt/homebrew/sbin",
}

// Find returns the first fixed folder that has the tool.
func Find(name string) (string, bool) {
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// ProgramFiles is Windows-only.
func ProgramFiles() string { return "" }
