//go:build !darwin

package main

import (
	"fmt"
	goruntime "runtime"
	"time"

	"fyne.io/systray"
)

// hideOnClose is macOS's native hide-on-close (see tray_darwin.go); here
// closing goes through beforeClose and the Close to tray setting.
const hideOnClose = false

// runTray shows the notification-area icon. systray needs its own locked OS
// thread on Windows, since it runs a separate message loop from Wails.
func (a *App) runTray() {
	goruntime.LockOSThread()
	systray.Run(a.onTrayReady, nil)
}

func quitTray() { systray.Quit() }

func (a *App) onTrayReady() {
	systray.SetIcon(trayIcon)
	systray.SetTooltip("Shark BiteTorrent")
	systray.SetOnTapped(a.showWindow)

	show := systray.AddMenuItem("Show", "Show the window")
	systray.AddSeparator()
	pauseAll := systray.AddMenuItem("Pause all", "Pause every torrent")
	resumeAll := systray.AddMenuItem("Resume all", "Resume every torrent")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "Stop all transfers and exit")

	go func() {
		for {
			select {
			case <-show.ClickedCh:
				a.showWindow()
			case <-pauseAll.ClickedCh:
				a.PauseAll()
			case <-resumeAll.ClickedCh:
				a.ResumeAll()
			case <-quit.ClickedCh:
				a.quit()
				return
			}
		}
	}()

	go func() {
		// NewTicker, not time.Tick: Tick's ticker is never collected.
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for range tick.C {
			if a.isShuttingDown() {
				return
			}
			systray.SetTooltip(a.trayTooltip())
		}
	}()
}

func (a *App) trayTooltip() string {
	vpn := a.GetVpnStatus()
	var down, up int64
	for _, t := range a.ListTorrents() {
		down += t.DownRate
		up += t.UpRate
	}
	state := "VPN on"
	switch vpn.State {
	case "connecting":
		state = "VPN connecting…"
	case "error":
		state = "VPN error, transfers stopped"
	}
	return fmt.Sprintf("Shark BiteTorrent · %s\n↓ %s/s  ↑ %s/s", state, human(down), human(up))
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
