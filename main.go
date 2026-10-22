package main

import (
	"embed"
	"fmt"
	"os"
	"strings"

	_ "a.local/earlyinit" // must stay: see package doc
	"sharkbite/internal/platform"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Started at sign-in (Settings → Desktop): run in the tray, no window.
	// A second launch (Start menu, a magnet link) shows the window.
	autostart := len(os.Args) == 2 && os.Args[1] == platform.AutostartArg

	// Called by the installer/uninstaller; do the work and exit without a window.
	if len(os.Args) == 2 && strings.HasPrefix(os.Args[1], "--") && !autostart {
		exe, _ := os.Executable()
		var err error
		switch os.Args[1] {
		case "--register-associations":
			err = platform.RegisterAssociations(exe)
		case "--unregister-associations":
			err = platform.UnregisterAssociations(exe)
		case "--remove-startup":
			err = platform.SetStartup(exe, false)
		default:
			err = fmt.Errorf("unknown option %s", os.Args[1])
		}
		if err != nil {
			os.Exit(1)
		}
		return
	}

	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "Shark BiteTorrent",
		Width:     1100,
		Height:    700,
		MinWidth:  720,
		MinHeight: 460,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour:  &options.RGBA{R: 24, G: 26, B: 31, A: 1},
		StartHidden:       autostart,
		OnStartup:         app.startup,
		OnShutdown:        app.shutdown,
		OnBeforeClose:     app.beforeClose,
		HideWindowOnClose: hideOnClose,
		// The browser's right-click menu (Cut, Copy, Paste) in text boxes and
		// on selected text only: Wails' "auto" rule hides it everywhere else,
		// and the torrent list has its own menu. Without it, pasting a magnet
		// link or a password took Ctrl+V.
		EnableDefaultContextMenu: true,
		// A second launch (e.g. clicking a magnet link while running) hands
		// its arguments to this instance instead of starting another.
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "sharkbitetorrent-3f6c2b1e",
			OnSecondInstanceLaunch: app.onSecondInstance,
		},
		// macOS delivers magnet links and opened .torrent files as events
		// (declared via "protocols"/"fileAssociations" in wails.json).
		Mac: &mac.Options{
			OnUrlOpen:  func(u string) { app.handleArgs([]string{u}, true) },
			OnFileOpen: func(p string) { app.handleArgs([]string{p}, true) },
		},
		DragAndDrop: &options.DragAndDrop{
			EnableFileDrop:     true,
			DisableWebViewDrop: true,
		},
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
