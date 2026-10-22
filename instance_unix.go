//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockInstance takes the lock only the running copy of the app holds (see
// App.secondary). It reports false if another copy holds it.
func (a *App) lockInstance() bool {
	os.MkdirAll(a.configDir, 0o700)
	f, err := os.OpenFile(filepath.Join(a.configDir, "instance.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return true // can't tell: carry on as before
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		f.Close()
		return false
	}
	a.instanceLock = f // held until the process exits
	return true
}
