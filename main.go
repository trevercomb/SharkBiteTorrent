package main

import (
	"context"
	"embed"
	"fmt"
	"os"
	"strings"

	_ "a.local/earlyinit" // must stay: see package doc
	"sharkbite/internal/platform"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/runtime"
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
		BackgroundColour: &options.RGBA{R: 24, G: 26, B: 31, A: 1},
		// macOS has no tray to bring back a window that was never shown, and
		// a Dock click only unhides a hidden app (Wails v2 has no reopen
		// hook), so there a sign-in start shows the window and hides the app
		// at once, as the close button does.
		StartHidden: autostart && !hideOnClose,
		OnStartup: func(ctx context.Context) {
			if autostart && hideOnClose {
				runtime.Hide(ctx)
			}
			app.startup(ctx)
		},
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
		// Linux: the app's icon on the window (X11), and a program name that
		// is the menu entry's file name, sharkbitetorrent.desktop: on Wayland
		// the window's app ID comes from it, and the desktop takes the icon
		// from the entry it names (it showed Wails' logo). Hardware
		// acceleration stays off, as Wails sets it when there are no Linux
		// options (rendering problems with some drivers, wails#2977).
		Linux: &linux.Options{
			Icon:             windowIcon,
			ProgramName:      "sharkbitetorrent",
			WebviewGpuPolicy: linux.WebviewGpuPolicyNever,
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
