package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sharkbite/internal/platform"
	"strconv"
	"strings"
	"time"

	"aead.dev/minisign"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// Update checks use GitHub's public "latest release" redirect instead of the
// REST API: the API allows 60 requests/hour per IP, and WARP users share IPs.
// Checks and downloads go through the VPN when it's up.

const (
	releasesURL    = "https://github.com/trevercomb/SharkBiteTorrent/releases"
	installerAsset = "SharkBiteTorrent-amd64-installer.exe"
	checksumsAsset = "SHA256SUMS.txt"
	signatureAsset = "SHA256SUMS.txt.minisig"
	// The automatic check: once the VPN is up if the last one was over an
	// hour ago (so restarting the app soon after a release finds it), then
	// every 20 hours while the app runs (it may run for weeks).
	autoCheckAtStart = time.Hour
	autoCheckPeriod  = 20 * time.Hour

	// Settings.UpdateChannel
	channelStable = "stable"
	channelBeta   = "beta"
)

type UpdateInfo struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	NotesURL  string `json:"notesURL"`
	// CanInstall is true where the app can download and run the installer
	// itself (the installed Windows version). Elsewhere the UI links to the
	// release page.
	CanInstall bool `json:"canInstall"`
	// CanDownload: Linux and macOS, where the app downloads the release's
	// file for this computer, checks it against the release signature and
	// saves it for the user to install (DownloadUpdate).
	CanDownload bool `json:"canDownload"`
	// Direct is true if the check went over the normal connection because
	// the VPN wasn't up.
	Direct bool `json:"direct"`
	// Notes are the release notes for every version after Current up to
	// Latest, newest first (empty if they couldn't be fetched).
	Notes []ReleaseNote `json:"notes"`
}

func (a *App) currentVersion() string { return a.GetAbout().Version }

// errVPNDown is returned when the VPN is down and the user hasn't agreed to
// use the normal connection; the frontend asks and retries with allowDirect.
var errVPNDown = errors.New("The VPN isn't connected")

// updateClient returns an HTTP client through the VPN, or the direct
// connection if allowDirect (the user agreed) and the VPN is down.
func (a *App) updateClient(allowDirect bool) (*http.Client, bool, error) {
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if tun != nil {
		c := tun.HTTPClient()
		c.Timeout = 10 * time.Minute // installer downloads
		return c, false, nil
	}
	if !allowDirect && !a.directMode() {
		return nil, false, errVPNDown
	}
	// Direct, as the user agreed: the system's own DNS, and no proxy from
	// the environment.
	d := &net.Dialer{Timeout: 30 * time.Second, Resolver: &net.Resolver{}}
	return &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 30 * time.Second}}, true, nil
}

func (a *App) checkUpdate(ctx context.Context, allowDirect bool) (UpdateInfo, error) {
	info := UpdateInfo{Current: a.currentVersion(), CanInstall: a.canInstallUpdates(), CanDownload: a.canDownloadUpdates()}
	hc, direct, err := a.updateClient(allowDirect)
	if err != nil {
		return info, err
	}
	info.Direct = direct

	// /releases/latest redirects to /releases/tag/vX.Y.Z; read the tag from
	// the redirect without following it.
	noFollow := *hc
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, releasesURL+"/latest", nil)
	if err != nil {
		return info, err
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		return info, fmt.Errorf("couldn't reach GitHub: %w", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return info, fmt.Errorf("no releases found (HTTP %d)", resp.StatusCode)
	}
	tag := loc[i+len("/tag/"):]
	info.Latest = strings.TrimPrefix(tag, "v")
	if !strings.HasPrefix(tag, "v") || !versionPattern.MatchString(info.Latest) {
		return info, fmt.Errorf("unexpected release tag %q", tag)
	}
	// The beta channel also looks at pre-releases (v0.3.0-beta.1), which
	// /latest never points to. They're installed only with the same signature
	// check as releases.
	if a.betaChannel() {
		if pre := newestPrerelease(ctx, hc); pre != "" && newerVersion(pre, info.Latest) {
			info.Latest = pre
			tag = "v" + pre
		}
	}
	info.NotesURL = releasesURL + "/tag/" + tag
	info.Available = newerVersion(info.Latest, info.Current)
	if info.Available {
		info.Notes = updateNotes(ctx, hc, info.Current, info.Latest)
	}
	return info, nil
}

func (a *App) betaChannel() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings.UpdateChannel == channelBeta
}

// releaseTagRe finds release tags in GitHub's releases feed (the newest 10
// releases, pre-releases included; drafts aren't in it).
// Only this repository's own release links count, as an href (not text in a
// release's description, nor another repository's links).
var releaseTagRe = regexp.MustCompile(`href="` + regexp.QuoteMeta(releasesURL) + `/tag/v([^"<>/\s]+)"`)

// newestPrerelease returns the newest pre-release version in the releases
// feed, or "" if there's none or the feed can't be read. The feed is a web
// page, not the REST API, so it isn't held to the API's per-IP limit.
func newestPrerelease(ctx context.Context, hc *http.Client) string {
	feed, err := fetchSmall(ctx, hc, releasesURL+".atom")
	if err != nil {
		return ""
	}
	return newestPrereleaseIn(feed)
}

func newestPrereleaseIn(feed []byte) string {
	var newest string
	for _, m := range releaseTagRe.FindAllSubmatch(feed, -1) {
		v := string(m[1])
		if strings.Contains(v, "-") && versionPattern.MatchString(v) && (newest == "" || newerVersion(v, newest)) {
			newest = v
		}
	}
	return newest
}

// newerVersion reports whether a > b for dotted numeric versions ("0.2.10" > "0.2.9").
// A pre-release suffix ("0.3.0-rc1") sorts before the release it leads up to.
func newerVersion(a, b string) bool {
	a, preA, _ := strings.Cut(a, "-")
	b, preB, _ := strings.Cut(b, "-")
	if a == b || !newerCore(a, b) && !newerCore(b, a) {
		switch {
		case preA == preB:
			return false
		case preA == "":
			return true // release beats its own pre-release
		case preB == "":
			return false
		}
		return newerPre(preA, preB)
	}
	return newerCore(a, b)
}

// newerPre compares pre-release suffixes, numerically where they differ
// only in a trailing number ("rc10" > "rc9").
func newerPre(a, b string) bool {
	ta, na := splitTrailingNumber(a)
	tb, nb := splitTrailingNumber(b)
	if ta == tb && na >= 0 && nb >= 0 {
		return na > nb
	}
	return a > b
}

func splitTrailingNumber(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return s, -1
	}
	n, err := strconv.Atoi(s[i:])
	if err != nil {
		return s, -1
	}
	return s[:i], n
}

func newerCore(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// autoCheckUpdate runs after the VPN connects: at most once per period,
// through the tunnel only (or directly if the user chose "No VPN"), and only
// if the user hasn't turned it off.
// autoCheckUpdates runs while the VPN is connected (ctx ends with it).
func (a *App) autoCheckUpdates(ctx context.Context) {
	a.autoCheckUpdate(autoCheckAtStart)
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.autoCheckUpdate(autoCheckPeriod)
		}
	}
}

// autoCheckUpdate checks for a new version if the last check was at least
// gap ago, and tells the window if there is one.
func (a *App) autoCheckUpdate(gap time.Duration) {
	a.mu.Lock()
	s := a.settings
	a.mu.Unlock()
	if storeBuild || !s.AutoUpdateCheck || time.Since(time.Unix(s.LastUpdateCheck, 0)) < gap {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := a.checkUpdate(ctx, false)
	if err != nil {
		return // quiet; the user can check manually
	}
	a.mu.Lock()
	a.settings.LastUpdateCheck = time.Now().Unix()
	skipped := a.settings.SkippedVersion
	a.mu.Unlock()
	a.writeSettings()
	if info.Available && info.Latest != skipped {
		runtime.EventsEmit(a.ctx, "update-available", info)
	}
}

// --- Bound to the frontend ---

// CheckForUpdates is the manual "Check for updates" button. It uses the VPN;
// if that's down it returns errVPNDown, unless allowDirect (the user agreed
// to the normal connection; reported via Direct).
func (a *App) CheckForUpdates(allowDirect bool) (UpdateInfo, error) {
	if storeBuild {
		return UpdateInfo{}, errors.New("the Microsoft Store installs updates for this version")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := a.checkUpdate(ctx, allowDirect)
	if err == nil {
		a.mu.Lock()
		a.settings.LastUpdateCheck = time.Now().Unix()
		a.mu.Unlock()
		a.writeSettings()
	}
	return info, err
}

func (a *App) SkipUpdate(version string) error {
	a.mu.Lock()
	a.settings.SkippedVersion = version
	a.mu.Unlock()
	return a.writeSettings()
}

// InstallUpdate downloads version's release file for this computer,
// verifies its SHA-256 against the release's signed SHA256SUMS.txt,
// installs it and quits: on Windows the installer runs silently and
// relaunches the app; on macOS and with the Linux tarball the app replaces
// itself and is started again once this copy has quit (applyUpdate).
// versionPattern is what a release version looks like ("0.2.24", "0.3.0-rc1").
// It's checked before the version goes into a download URL.
var versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}(-[0-9A-Za-z.]+)?$`)

// allowDirect is as for CheckForUpdates: without it, a VPN that's down
// returns errVPNDown instead of downloading over the normal connection.
func (a *App) InstallUpdate(version string, allowDirect bool) (err error) {
	if !a.installing.CompareAndSwap(false, true) {
		return errors.New("the update is already being installed")
	}
	defer func() {
		if err != nil {
			a.installing.Store(false) // failed: it can be tried again
		}
	}()
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("not a version number: %q", version)
	}
	if !newerVersion(version, a.currentVersion()) {
		return fmt.Errorf("%s isn't newer than this version (%s)", version, a.currentVersion())
	}
	if !a.canInstallUpdates() {
		return errors.New("automatic install isn't available for this version; download it from the release page")
	}
	hc, _, err := a.updateClient(allowDirect)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	base := releasesURL + "/download/v" + version + "/"
	asset := selfUpdateAsset(version, a.exe)

	want, err := signedChecksum(ctx, hc, base, "v"+version, asset)
	if err != nil {
		return fmt.Errorf("couldn't verify the release: %w", err)
	}
	dir, err := os.MkdirTemp("", "sharkbite-update-")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, asset)
	got, err := download(ctx, hc, base+asset, path)
	if err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("download failed: %w", err)
	}
	if !strings.EqualFold(got, want) {
		os.RemoveAll(dir)
		return errors.New("the downloaded update failed its checksum check; not installing")
	}
	if err := applyUpdate(path, version, a.exe); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("installing the update: %w", err)
	}
	if goruntime.GOOS != "windows" {
		os.RemoveAll(dir) // the installer runs from it on Windows; cleaned up at the next start
	}
	a.quit()
	return nil
}

// releasePub is the maintainer's release-signing public key (minisign). The
// private key is kept offline, never on GitHub, so a compromised GitHub
// account can't publish an update this app will install.
//
//go:embed release.pub
var releasePub []byte

// signedChecksum fetches the release's SHA256SUMS.txt and its signature,
// checks the signature against releasePub and that it was made for this tag
// (so an older signed release can't be passed off as a newer one), and
// returns the checksum listed for name.
func signedChecksum(ctx context.Context, hc *http.Client, base, tag, name string) (string, error) {
	var pub minisign.PublicKey
	if err := pub.UnmarshalText(bytes.TrimSpace(releasePub)); err != nil {
		return "", fmt.Errorf("bad built-in key: %w", err)
	}
	sums, err := fetchSmall(ctx, hc, base+checksumsAsset)
	if err != nil {
		return "", fmt.Errorf("checksums: %w", err)
	}
	sig, err := fetchSmall(ctx, hc, base+signatureAsset)
	if err != nil {
		return "", fmt.Errorf("signature: %w", err)
	}
	return verifyChecksum(pub, sums, sig, tag, name)
}

func verifyChecksum(pub minisign.PublicKey, sums, sig []byte, tag, name string) (string, error) {
	if !minisign.Verify(pub, sums, sig) {
		return "", errors.New("the release's signature doesn't match; not installing it")
	}
	var s minisign.Signature
	if err := s.UnmarshalText(sig); err != nil {
		return "", err
	}
	if s.TrustedComment != "SharkBiteTorrent "+tag {
		return "", fmt.Errorf("the signature is for %q, not %s", s.TrustedComment, tag)
	}
	return checksumFor(sums, name)
}

func fetchSmall(ctx context.Context, hc *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// checksumFor finds name in sha256sum output ("<hex>  <name>"; binary mode
// prefixes the name with "*").
func checksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name && len(fields[0]) == 64 {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%s not listed", name)
}

// download saves url to path and returns its SHA-256 hex digest.
func download(ctx context.Context, hc *http.Client, url, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 500<<20)); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canInstallUpdates reports whether "Update now" may install the official
// release. Not in nofunding builds: the official release has the funding
// links back in, so those users get a link to rebuild or download instead.
func (a *App) canInstallUpdates() bool {
	return fundingEnabled && !storeBuild && canSelfUpdate(a.exe)
}

func (a *App) canDownloadUpdates() bool {
	return fundingEnabled && !storeBuild && updateAsset("0", a.exe) != ""
}

// DownloadedUpdate is a verified release file saved for the user.
type DownloadedUpdate struct {
	Path string `json:"path"`
	Hint string `json:"hint"` // how to install it
	// Command: the command that installs it, if it's installed by one (the
	// pacman package), shown with a Copy button.
	Command string `json:"command"`
}

// DownloadUpdate (Linux, macOS) downloads version's file for this computer,
// checks it against the signed SHA256SUMS.txt like InstallUpdate does, saves
// it in Downloads and shows it. allowDirect is as for InstallUpdate.
func (a *App) DownloadUpdate(version string, allowDirect bool) (DownloadedUpdate, error) {
	var out DownloadedUpdate
	if !versionPattern.MatchString(version) {
		return out, fmt.Errorf("not a version number: %q", version)
	}
	if !newerVersion(version, a.currentVersion()) {
		return out, fmt.Errorf("%s isn't newer than this version (%s)", version, a.currentVersion())
	}
	asset := updateAsset(version, a.exe)
	if !a.canDownloadUpdates() || asset == "" {
		return out, errors.New("download it from the release page")
	}
	hc, _, err := a.updateClient(allowDirect)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	base := releasesURL + "/download/v" + version + "/"
	want, err := signedChecksum(ctx, hc, base, "v"+version, asset)
	if err != nil {
		return out, fmt.Errorf("couldn't verify the release: %w", err)
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return out, err
	}
	// A temporary file of its own (not following anything already there),
	// renamed to a name that's free: nothing in Downloads is overwritten.
	f, err := os.CreateTemp(dir, asset+".*.download")
	if err != nil {
		return out, err
	}
	tmp := f.Name()
	f.Close()
	// Remembered so a download cut off by the app quitting is cleaned up at
	// the next start, without that having to look through Downloads.
	a.markDownload(tmp, true)
	defer a.markDownload(tmp, false)
	got, err := download(ctx, hc, base+asset, tmp)
	if err != nil {
		os.Remove(tmp)
		return out, fmt.Errorf("download failed: %w", err)
	}
	if !strings.EqualFold(got, want) {
		os.Remove(tmp)
		return out, errors.New("the download didn't match the release's signed checksum, so it was deleted")
	}
	// os.Link fails if the name was taken meanwhile (rename would replace it).
	var final string
	for {
		final = freeName(dir, asset)
		err := os.Link(tmp, final)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			os.Remove(tmp)
			return out, err
		}
	}
	os.Remove(tmp)
	// Not opened in the file manager by itself: its window covered the
	// instructions (see ShowDownloadedUpdate).
	a.mu.Lock()
	a.downloadedUpdate = final
	a.mu.Unlock()
	return DownloadedUpdate{Path: final, Hint: installHint(final), Command: installCommand(final)}, nil
}

// ShowDownloadedUpdate shows the update file DownloadUpdate saved in the
// file manager (only that file).
func (a *App) ShowDownloadedUpdate() error {
	a.mu.Lock()
	p := a.downloadedUpdate
	a.mu.Unlock()
	if p == "" {
		return errors.New("no update has been downloaded")
	}
	return platform.RevealPath(p, false)
}

func (a *App) updateDownloadMarker() string {
	return filepath.Join(a.configDir, "update-download")
}

// cleanUpdateTemp removes installer folders left by earlier updates (the
// installer is still running from its folder when the app quits).
func (a *App) cleanUpdateTemp() {
	cleanUpdateLeftovers(a.exe)
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), "sharkbite-update-*"))
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && time.Since(fi.ModTime()) > time.Hour {
			os.RemoveAll(d)
		}
	}
	// And unfinished update downloads (Linux, macOS) left in Downloads by
	// the app quitting during them. Only files DownloadUpdate recorded are
	// touched: on macOS, even listing Downloads asks the user for permission.
	a.markerMu.Lock()
	defer a.markerMu.Unlock()
	var keep []string
	for _, f := range a.readMarker() {
		fi, err := os.Lstat(f)
		switch {
		case err != nil: // already gone
		case !updateDownloadName(f) || !fi.Mode().IsRegular(): // not a download of ours: leave it
		case time.Since(fi.ModTime()) <= time.Hour:
			keep = append(keep, f) // may still be downloading: look again next time
		default:
			os.Remove(f)
		}
	}
	a.writeMarker(keep)
}

// updateDownloadName reports whether f is an update download's temporary
// file, directly in Downloads (the marker is a file anyone running as this
// user could edit; nothing elsewhere is ever deleted on its word).
func updateDownloadName(f string) bool {
	home, err := os.UserHomeDir()
	if err != nil || filepath.Dir(f) != filepath.Join(home, "Downloads") {
		return false
	}
	name := filepath.Base(f)
	return strings.HasSuffix(name, ".download") &&
		(strings.HasPrefix(name, "sharkbitetorrent-") || strings.HasPrefix(name, "SharkBiteTorrent-"))
}

// markDownload adds tmp to the update downloads in progress, or removes it.
func (a *App) markDownload(tmp string, add bool) {
	a.markerMu.Lock()
	defer a.markerMu.Unlock()
	var list []string
	for _, f := range a.readMarker() {
		if f != tmp {
			list = append(list, f)
		}
	}
	if add {
		list = append(list, tmp)
	}
	a.writeMarker(list)
}

func (a *App) readMarker() []string {
	b, _ := os.ReadFile(a.updateDownloadMarker())
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (a *App) writeMarker(list []string) {
	if len(list) == 0 {
		os.Remove(a.updateDownloadMarker())
		return
	}
	os.WriteFile(a.updateDownloadMarker(), []byte(strings.Join(list, "\n")+"\n"), 0o600)
}

// freeName is dir/name, or "name (2)", "name (3)"… if that's taken.
func freeName(dir, name string) string {
	base, ext := name, ""
	for _, e := range []string{".pkg.tar.zst", ".tar.gz", ".zip"} {
		if b, ok := strings.CutSuffix(name, e); ok {
			base, ext = b, e
			break
		}
	}
	p := filepath.Join(dir, name)
	for i := 2; ; i++ {
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
}
