package main

import (
	"fmt"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"sharkbite/internal/engine"
)

// announceFinished shows a system notification that a download finished,
// with what the malware scan found (Settings.NotifyFinished). It runs once
// its checks are done, so it doesn't say "ready" about a file that turns
// out to hold a threat.
func (a *App) announceFinished(name string, rec *scanRecord) {
	a.mu.Lock()
	on := a.settings.NotifyFinished
	a.mu.Unlock()
	if !on {
		return
	}
	title, body := finishedMessage(name, rec)
	go func() { // on macOS the first one can wait for the user to allow them
		if err := a.notify(title, body); err != nil {
			notifyFailed.Do(func() {
				runtime.LogWarningf(a.ctx, "%s", engine.Redact(fmt.Sprintf("notification: %v", err)))
			})
		}
	}()
}

// notifyFailed: a failure is logged once, not for every download.
var notifyFailed sync.Once

func finishedMessage(name string, rec *scanRecord) (title, body string) {
	switch {
	case len(rec.Threats) > 0:
		return "⚠ Threat found", fmt.Sprintf("%s found a threat in “%s”. Don't open its files.", rec.Engine, name)
	case rec.SFV != nil && len(rec.SFV.Bad) > 0:
		return "⚠ Download finished with errors", fmt.Sprintf("“%s”: some files don't match its .sfv checksums.", name)
	case rec.Clean && len(rec.TooLarge) > 0:
		return "Download finished", fmt.Sprintf("“%s” is ready. Some files were too large for %s to scan.", name, rec.Engine)
	case rec.Clean:
		return "Download finished", fmt.Sprintf("“%s” is ready. %s found no threats.", name, rec.Engine)
	}
	return "Download finished", fmt.Sprintf("“%s” is ready.", name)
}
