package main

import (
	_ "embed"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"git.sr.ht/~jackmordaunt/go-toast/v2/wintoast"
	"golang.org/x/sys/windows/registry"
)

//go:embed build/appicon.png
var notifyIcon []byte

// toastAppID: notifications need an app ID Windows knows. The Store
// package has one (its name and icon come from the package, and clicking a
// notification opens the app); otherwise it's registered under HKCU, which
// the uninstaller removes.
const toastAppID = "SharkBiteTorrent"

var (
	toastSetup sync.Once
	toastID    string
	toastErr   error
)

func (a *App) notify(title, body string) error {
	toastSetup.Do(func() { toastID, toastErr = a.toastAppID() })
	if toastErr != nil {
		return toastErr
	}
	return wintoast.Push(toastID, `<toast><visual><binding template="ToastGeneric"><text>`+
		escapeXML(title)+`</text><text>`+escapeXML(body)+`</text></binding></visual></toast>`)
}

func (a *App) toastAppID() (string, error) {
	if family := packageFamily(); family != "" {
		return family + "!SharkBiteTorrent", nil // the Application Id in build/msix/AppxManifest.xml
	}
	icon := filepath.Join(a.configDir, "notify-icon.png")
	if err := os.WriteFile(icon, notifyIcon, 0o644); err != nil {
		return "", err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\AppUserModelId\`+toastAppID, registry.SET_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	if err := k.SetStringValue("DisplayName", "Shark BiteTorrent"); err != nil {
		return "", err
	}
	return toastAppID, k.SetStringValue("IconUri", icon)
}

// packageFamily is the Store package's family name, or "" when the app
// isn't running from a package.
func packageFamily() string {
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetCurrentPackageFamilyName")
	if proc.Find() != nil {
		return ""
	}
	n := uint32(256)
	buf := make([]uint16, n)
	if r, _, _ := proc.Call(uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
		return "" // APPMODEL_ERROR_NO_PACKAGE
	}
	return syscall.UTF16ToString(buf)
}

func escapeXML(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
