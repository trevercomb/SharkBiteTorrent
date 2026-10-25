package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"sharkbite/internal/engine"
	"sharkbite/internal/vpnprov"
)

func TestExistingAncestor(t *testing.T) {
	root := t.TempDir()
	if got := existingAncestor(root); got != root {
		t.Errorf("existing dir: got %q", got)
	}
	missing := filepath.Join(root, "Torrents", "deeper")
	if got := existingAncestor(missing); got != root {
		t.Errorf("deleted folder: got %q, want %q", got, root)
	}
	if got := existingAncestor(""); got != "" {
		t.Errorf("empty: got %q", got)
	}
}

// A magnet from outside the app is only remembered: no engine is involved
// (so no tracker, DHT or peer is contacted) until the user starts it.
func TestExternalMagnetWaits(t *testing.T) {
	a := &App{configDir: t.TempDir()}
	a.settings.DownloadDir = t.TempDir()
	const ih = "0123456789abcdef0123456789abcdef01234567"
	link := "magnet:?xt=urn:btih:" + ih + "&dn=Some+Linux.iso&tr=https://tracker.example/announce"
	if err := a.addSourceFrom(link, true); err != nil {
		t.Fatal(err)
	}
	if a.eng != nil {
		t.Fatal("an engine was started")
	}
	list := a.ListTorrents()
	if len(list) != 1 || list[0].Hash != ih || list[0].Name != "Some Linux.iso" || !list[0].Confirm {
		t.Fatalf("list: %+v", list)
	}
	if fs, err := a.GetFiles(ih); err != nil || len(fs) != 0 {
		t.Fatalf("files: %v %v", fs, err)
	}
	if err := a.Remove(ih, true); err != nil {
		t.Fatal(err)
	}
	if len(a.ListTorrents()) != 0 {
		t.Fatal("still listed after Remove")
	}
}

// An outside .torrent shows its file-list warnings while it waits, before
// anything is downloaded.
func TestExternalTorrentWarnings(t *testing.T) {
	a := &App{configDir: t.TempDir()}
	a.settings.DownloadDir = t.TempDir()
	info := metainfo.Info{Name: "Movie", PieceLength: 16 << 10, Pieces: make([]byte, 20), Files: []metainfo.FileInfo{
		{Path: []string{"movie.mkv"}, Length: 1 << 30},
		{Path: []string{"movie.mp4.exe"}, Length: 1000},
	}}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "movie.torrent")
	f, _ := os.Create(src)
	(&metainfo.MetaInfo{InfoBytes: ib}).Write(f)
	f.Close()
	if err := a.addSourceFrom(src, true); err != nil {
		t.Fatal(err)
	}
	list := a.ListTorrents()
	if len(list) != 1 || !list[0].Confirm || len(list[0].Warnings) == 0 {
		t.Fatalf("list: %+v", list)
	}
	// A file that isn't a valid .torrent is refused, not saved blank.
	bad := filepath.Join(t.TempDir(), "bad.torrent")
	os.WriteFile(bad, []byte("d4:infoi1ee"), 0o644)
	if err := a.addSourceFrom(bad, true); err == nil {
		t.Error("broken .torrent accepted")
	}
}

// A torrent waiting from an outside add, then added from inside the app, is
// checked like any new one, also after a restart: never adopted as if an
// older version had saved it (the audit's sequence).
func TestAddedTorrentsNeverLegacy(t *testing.T) {
	a := &App{configDir: t.TempDir()}
	a.settings.DownloadDir = t.TempDir()
	const ih = "0123456789abcdef0123456789abcdef01234567"
	link := "magnet:?xt=urn:btih:" + ih + "&dn=x"
	if err := a.addSourceFrom(link, true); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir() // the download folder changed meanwhile
	if err := a.recordAdded(ih, link, other); err != nil {
		t.Fatal(err)
	}
	b := &App{configDir: a.configDir} // restart
	b.loadSaved()
	if len(b.saved) != 1 || b.saved[0].legacy || !b.saved[0].Checked || b.saved[0].Confirm || b.saved[0].Dir != other {
		t.Fatalf("after restart: %+v", b.saved)
	}

	// An entry written by v0.2.36 (no "checked" field) is the only legacy one.
	os.WriteFile(filepath.Join(a.configDir, "torrents.json"),
		[]byte(`[{"hash":"`+ih+`","source":"`+link+`","dir":"/x","paused":false}]`), 0o600)
	c := &App{configDir: a.configDir}
	c.loadSaved()
	if len(c.saved) != 1 || !c.saved[0].legacy {
		t.Fatalf("v0.2.36 entry not legacy: %+v", c.saved)
	}
}

// The download folder is created, but not missing folders above it (a
// drive that isn't connected).
func TestEnsureDownloadDir(t *testing.T) {
	base := t.TempDir()
	home := t.TempDir()
	old := userHome
	userHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHome = old })
	if err := ensureDownloadDir(filepath.Join(base, "Torrents")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(base, "Torrents")); err != nil || !fi.IsDir() {
		t.Error("the folder wasn't created")
	}
	err := ensureDownloadDir(filepath.Join(base, "Volumes", "Public", "Plex"))
	if err == nil || !strings.Contains(err.Error(), "isn't available") {
		t.Errorf("a folder on a drive that isn't there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "Volumes")); !os.IsNotExist(err) {
		t.Error("created folders above the download folder")
	}
	// Inside the home folder, missing parents are created (the default
	// ~/Downloads/Torrents with no ~/Downloads).
	def := filepath.Join(home, "Downloads", "Torrents")
	if err := ensureDownloadDir(def); err != nil {
		t.Fatalf("default folder without ~/Downloads: %v", err)
	}
	if fi, err := os.Stat(def); err != nil || !fi.IsDir() {
		t.Error("the default folder wasn't created")
	}
	if insideHome(home) {
		t.Error("the home folder itself counted as inside it")
	}
}

// A .torrent from outside the app whose file list has warnings, and whose
// top folder is named like a film, isn't started from the phone (the phone
// page doesn't show warnings), nor by Resume all.
func TestWarnedTorrentNotStartedRemotely(t *testing.T) {
	a := &App{configDir: t.TempDir()}
	a.settings.DownloadDir = t.TempDir()
	info := metainfo.Info{Name: "Movie.2026.1080p.WEB-DL", PieceLength: 16 << 10, Pieces: make([]byte, 20), Files: []metainfo.FileInfo{
		{Path: []string{"setup.exe"}, Length: 1000},
		{Path: []string{"readme.txt"}, Length: 10},
	}}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "movie.torrent")
	f, _ := os.Create(src)
	(&metainfo.MetaInfo{InfoBytes: ib}).Write(f)
	f.Close()
	if err := a.addSourceFrom(src, true); err != nil {
		t.Fatal(err)
	}
	list := a.ListTorrents()
	if len(list) != 1 || len(list[0].Warnings) == 0 {
		t.Fatalf("a program in a folder named like a film isn't flagged: %+v", list)
	}
	if err := a.resumeRemote(list[0].Hash); !errors.Is(err, engine.ErrWarnings) {
		t.Errorf("phone resume: %v, want ErrWarnings", err)
	}
	a.ResumeAll() // skips it (added from outside, and warned)
	if l := a.ListTorrents(); !l[0].Confirm {
		t.Error("Resume all started it")
	}
}

// Auto verify defaults to suspect downloads only, also for settings saved
// before the option existed.
func TestAutoVerifyMode(t *testing.T) {
	a := &App{}
	for in, want := range map[string]string{"": "suspect", "suspect": "suspect", "all": "all", "off": "off", "bogus": "suspect"} {
		a.settings.AutoVerify = in
		if got := a.autoVerifyMode(); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// Only one copy holds the instance lock; a second launch (on Linux, Wails
// runs its startup before handing it to the running copy) neither marks the
// torrent list nor removes the running copy's marker when it exits.
func TestSecondInstanceLeavesStateAlone(t *testing.T) {
	dir := t.TempDir()
	first := &App{configDir: dir}
	if !first.lockInstance() {
		t.Fatal("first copy didn't get the lock")
	}
	defer first.instanceLock.Close()
	os.WriteFile(first.runningMarker(), nil, 0o600)
	second := &App{configDir: dir}
	if second.lockInstance() {
		t.Fatal("second copy got the lock too")
	}
	second.secondary = true
	second.stopAllNow(t.Context())
	if _, err := os.Stat(first.runningMarker()); err != nil {
		t.Error("the second copy removed the running copy's marker")
	}
}

func TestRateLimitWait(t *testing.T) {
	a := &App{}
	limited := func(after time.Duration) error {
		return fmt.Errorf("PIA: %w", &vpnprov.RateLimitError{RetryAfter: after})
	}
	if err := a.noteRateLimit("pia", errors.New("other")); err.Error() != "other" {
		t.Fatalf("other errors pass through: %v", err)
	}
	// No Retry-After: 10 minutes, doubling, up to an hour.
	for _, want := range []time.Duration{10, 20, 40, 60, 60} {
		err := a.noteRateLimit("pia", limited(0))
		var rle *rateLimitError
		if !errors.As(err, &rle) || !strings.Contains(err.Error(), "Private Internet Access") {
			t.Fatalf("message: %v", err)
		}
		if got := time.Until(a.limitUntil).Round(time.Minute); got != want*time.Minute {
			t.Errorf("wait %v, want %v min", got, want)
		}
	}
	if a.rateLimitedNow("pia") == nil || a.rateLimitedNow("mullvad") != nil {
		t.Error("only the limited provider is held back")
	}
	a.noteRateLimit("pia", limited(2*time.Minute)) // the provider's own wait wins
	if got := time.Until(a.limitUntil).Round(time.Minute); got != 2*time.Minute {
		t.Errorf("Retry-After: %v", got)
	}
	a.clearRateLimit("pia")
	if a.rateLimitedNow("pia") != nil || a.limitStrikes != 0 || a.limitTimer != nil {
		t.Error("not cleared after a connection")
	}
}

// A refreshed session token isn't a change of account.
func TestAccountKeyIgnoresCache(t *testing.T) {
	acc := &vpnprov.Account{Data: map[string]string{"username": "u", "password": "p"}}
	k := accountKey(acc)
	acc.Data[vpnprov.CachePrefix+"token"] = "t"
	if accountKey(acc) != k {
		t.Error("token changed the account key")
	}
	acc.Data["password"] = "q"
	if accountKey(acc) == k {
		t.Error("password change not seen")
	}
}
