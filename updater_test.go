package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"
)

func TestNewerVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.2.1", "0.2.0", true},
		{"0.2.10", "0.2.9", true},
		{"0.3", "0.2.9", true},
		{"1.0.0", "0.9.9", true},
		{"0.2.0", "0.2.0", false},
		{"0.2.0", "0.2.1", false},
		{"0.2", "0.2.0", false},
		{"0.3.0", "0.3.0-rc1", true},
		{"0.3.0-rc1", "0.3.0", false},
		{"0.3.0-rc2", "0.3.0-rc1", true},
		{"0.3.0-rc10", "0.3.0-rc9", true},
		{"0.3.0-rc9", "0.3.0-rc10", false},
		{"0.3.0-rc1", "0.2.18", true},
		{"0.2.18", "0.3.0-rc1", false},
		{"0.2.41-beta.10", "0.2.41-beta.9", true},
		{"0.2.41-beta.2", "0.2.41-beta.1", true},
		{"0.2.41", "0.2.41-beta.3", true},
		{"0.2.41-beta.1", "0.2.40", true},
	}
	for _, c := range cases {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func TestVersionPattern(t *testing.T) {
	for _, v := range []string{"0.2.24", "1.0", "0.3.0-rc1", "1.2.3.4"} {
		if !versionPattern.MatchString(v) {
			t.Errorf("rejected %q", v)
		}
	}
	for _, v := range []string{"", "1", "../../x", "0.2.24/../../evil", "1.2.3?x=1", "v1.2.3", "1.2.3 "} {
		if versionPattern.MatchString(v) {
			t.Errorf("accepted %q", v)
		}
	}
}

func TestVerifyChecksum(t *testing.T) {
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte("aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111  SharkBiteTorrent.exe\n" +
		"bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222  SharkBiteTorrent-amd64-installer.exe\n")
	sig := minisign.SignWithComments(priv, sums, "SharkBiteTorrent v0.3.0", "")

	got, err := verifyChecksum(pub, sums, sig, "v0.3.0", installerAsset)
	if err != nil || got != "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := verifyChecksum(pub, sums, sig, "v0.3.0", "missing.exe"); err == nil {
		t.Error("unlisted file accepted")
	}
	// An older genuine release re-uploaded under a newer tag.
	if _, err := verifyChecksum(pub, sums, sig, "v0.4.0", installerAsset); err == nil {
		t.Error("signature for another version accepted")
	}
	// Tampered checksums.
	bad := bytes.Replace(sums, []byte("bbbb"), []byte("cccc"), 1)
	if _, err := verifyChecksum(pub, bad, sig, "v0.3.0", installerAsset); err == nil {
		t.Error("tampered checksums accepted")
	}
	// Signed by someone else.
	_, other, _ := minisign.GenerateKey(rand.Reader)
	if _, err := verifyChecksum(pub, sums, minisign.SignWithComments(other, sums, "SharkBiteTorrent v0.3.0", ""), "v0.3.0", installerAsset); err == nil {
		t.Error("signature from another key accepted")
	}
}

func TestReleaseKeyEmbedded(t *testing.T) {
	var pub minisign.PublicKey
	if err := pub.UnmarshalText(bytes.TrimSpace(releasePub)); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%X", pub.ID()) != "F59273E544BB8BA3" {
		t.Errorf("embedded key %X", pub.ID())
	}
}

func TestCleanUpdateTempOnlyRecordedFile(t *testing.T) {
	a := &App{configDir: t.TempDir()}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows
	dl := filepath.Join(home, "Downloads")
	os.MkdirAll(dl, 0o755)
	old := time.Now().Add(-2 * time.Hour)
	write := func(dir, name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
		return p
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	// Two downloads cut off by quitting, a third still going; and files the
	// marker names but that aren't update downloads in Downloads.
	stale1 := write(dl, "SharkBiteTorrent-macos-universal.zip.1.download", old)
	stale2 := write(dl, "sharkbitetorrent-x.pkg.tar.zst.2.download", old)
	fresh := write(dl, "SharkBiteTorrent-macos-universal.zip.3.download", time.Now())
	other := write(dl, "SharkBiteTorrent-macos-universal.zip.4.download", old) // not recorded
	elsewhere := write(t.TempDir(), "SharkBiteTorrent-x.zip.5.download", old)
	notOurs := write(dl, "holiday.mov", old)
	for _, f := range []string{stale1, stale2, fresh, elsewhere, notOurs} {
		a.markDownload(f, true)
	}
	a.markDownload(fresh, true) // recorded once only
	a.cleanUpdateTemp()
	if exists(stale1) || exists(stale2) {
		t.Error("a recorded stale download wasn't removed")
	}
	if !exists(fresh) || !exists(other) || !exists(elsewhere) || !exists(notOurs) {
		t.Error("removed a file it shouldn't have (still downloading, unrecorded, outside Downloads, or not an update)")
	}
	if got := a.readMarker(); len(got) != 1 || got[0] != fresh {
		t.Errorf("marker after cleanup: %v (only the download that may still be going should stay)", got)
	}
	a.markDownload(fresh, false)
	if _, err := os.Stat(a.updateDownloadMarker()); !os.IsNotExist(err) {
		t.Error("the marker wasn't removed once nothing was left in it")
	}
}

func TestPrereleasesIn(t *testing.T) {
	feed := `<feed>
<entry><link rel="alternate" type="text/html" href="https://github.com/trevercomb/SharkBiteTorrent/releases/tag/v0.2.41"/></entry>
<entry><link rel="alternate" type="text/html" href="https://github.com/trevercomb/SharkBiteTorrent/releases/tag/v0.2.41-beta.9"/></entry>
<entry><link rel="alternate" type="text/html" href="https://github.com/trevercomb/SharkBiteTorrent/releases/tag/v0.2.41-beta.10"/></entry>
<entry><link rel="alternate" type="text/html" href="https://github.com/trevercomb/SharkBiteTorrent/releases/tag/v0.2.42-beta.1;rm"/></entry>
<entry><link rel="alternate" type="text/html" href="https://github.com/trevercomb/SharkBiteTorrent/releases/tag/v0.2.40"/></entry>
</feed>`
	// Newest first: the caller tries them in order until one is published.
	if got := strings.Join(prereleasesIn([]byte(feed)), " "); got != "0.2.41-beta.10 0.2.41-beta.9" {
		t.Errorf("got %q", got)
	}
	if got := prereleasesIn([]byte(`<feed><link href="https://github.com/x/releases/tag/v0.2.40"/></feed>`)); len(got) != 0 {
		t.Errorf("no pre-release: got %q", got)
	}
	// Only this repository's links, as hrefs: not another repository's, nor
	// a link written in a release's description.
	other := `<feed><entry><link href="https://github.com/someone/fork/releases/tag/v9.9.9-beta.1"/>
<content>see https://github.com/trevercomb/SharkBiteTorrent/releases/tag/v9.9.8-beta.1" here</content></entry></feed>`
	if got := prereleasesIn([]byte(other)); len(got) != 0 {
		t.Errorf("foreign link taken: %q", got)
	}
}

// A beta is installed or downloaded only on the Beta channel, whatever
// offered it (a banner from before the channel changed, say).
func TestCheckChannel(t *testing.T) {
	a := &App{}
	if err := a.checkChannel("0.2.42-beta.1"); err == nil {
		t.Error("a beta was allowed on Stable")
	}
	if err := a.checkChannel("0.2.42"); err != nil {
		t.Errorf("a release was refused on Stable: %v", err)
	}
	a.settings.UpdateChannel = channelBeta
	if err := a.checkChannel("0.2.42-beta.1"); err != nil {
		t.Errorf("a beta was refused on Beta: %v", err)
	}
}
