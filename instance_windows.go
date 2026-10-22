package main

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockInstance takes the lock only the running copy of the app holds (see
// App.secondary). It reports false if another copy holds it.
func (a *App) lockInstance() bool {
	os.MkdirAll(a.configDir, 0o700)
	f, err := os.OpenFile(filepath.Join(a.configDir, "instance.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return true // can't tell: carry on as before
	}
	var ol windows.Overlapped
	if windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol) != nil {
		f.Close()
		return false
	}
	a.instanceLock = f // held until the process exits
	return true
}
