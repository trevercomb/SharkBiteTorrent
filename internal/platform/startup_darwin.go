package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"sharkbite/internal/systool"
)

// macOS starts the launch agents in ~/Library/LaunchAgents with RunAtLoad at
// sign-in (no admin, no extra frameworks).

const agentLabel = "com.sharkbite.torrent.autostart"

func agentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", agentLabel+".plist")
}

var (
	agentProgram = regexp.MustCompile(`<array>\s*<string>([^<]*)</string>`)
	agentPrint   = regexp.MustCompile(`<key>` + printKey + `</key>\s*<string>([0-9a-f]*)</string>`)
)

// printKey holds a fingerprint (SHA-256) of the program the agent was set
// up for (launchd ignores keys it doesn't know). macOS 13 and later tie a
// launch agent to the program it first started (a launch constraint): for
// an app without a Developer ID that's the program's exact code, which
// every update changes, so the first sign-in after an update killed the
// app ("OS_REASON_CODESIGNING"). When the fingerprint differs, the agent
// is registered again (RefreshStartup), which takes the new program.
const printKey = "SharkBiteTorrentProgramSHA256"

func fingerprint(exe string) string {
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// reloadAgent registers the agent with launchd again (it then starts a copy
// at once, RunAtLoad, which hands its --autostart to the running app and
// exits). A var for tests.
var reloadAgent = func() {
	domain := "gui/" + strconv.Itoa(os.Getuid())
	systool.Command("launchctl", "bootout", domain+"/"+agentLabel).Run()
	systool.Command("launchctl", "bootstrap", domain, agentPath()).Run()
}

func StartupEnabled() bool {
	_, err := os.Stat(agentPath())
	return err == nil
}

func SetStartup(exe string, on bool) error {
	p := agentPath()
	if !on {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>%s</key>
	<string>%s</string>
</dict>
</plist>
`, agentLabel, html.EscapeString(exe), AutostartArg, printKey, fingerprint(exe))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(plist), 0o644)
}

// RefreshStartup keeps sign-in start working: if the app moved, the agent
// points at where it is now; if its program changed (an update), the agent
// is registered again for it (see printKey). Not while running as the
// agent's own process (started with --autostart): unloading the agent
// would stop it, and the program it started matches anyway.
func RefreshStartup(exe string) {
	b, err := os.ReadFile(agentPath())
	if err != nil {
		return
	}
	m := agentProgram.FindSubmatch(b)
	if m == nil {
		return
	}
	program := html.UnescapeString(string(m[1]))
	if _, err := os.Stat(program); err != nil {
		if !inWorldWritableDir(exe) {
			SetStartup(exe, true)
		}
		return
	}
	if program != exe || startedAtSignIn() {
		return
	}
	var recorded string
	if pm := agentPrint.FindSubmatch(b); pm != nil {
		recorded = string(pm[1])
	}
	if now := fingerprint(exe); now != "" && now != recorded {
		if SetStartup(exe, true) == nil {
			reloadAgent()
		}
	}
}

// startedAtSignIn reports whether this process is the launch agent's.
var startedAtSignIn = func() bool {
	return len(os.Args) == 2 && os.Args[1] == AutostartArg
}
