package engine

import (
	"bytes"
	"crypto/sha1"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func multiInfo(name string, files ...[]string) *metainfo.Info {
	info := &metainfo.Info{Name: name, PieceLength: 16 << 10, Pieces: make([]byte, 20)}
	for _, f := range files {
		info.Files = append(info.Files, metainfo.FileInfo{Path: f, Length: 1})
	}
	return info
}

func TestTorrentPathsRejectsUnsafeNames(t *testing.T) {
	dir := t.TempDir()
	bad := []*metainfo.Info{
		{Name: "", Length: 1},
		{Name: ".", Length: 1},
		{Name: "..", Length: 1},
		{Name: ".bashrc", Length: 1},    // hidden top-level name: the user's own dotfiles
		{Name: "a:b", Length: 1},        // NTFS alternate data stream
		{Name: "movie.exe.", Length: 1}, // Windows drops the trailing dot
		{Name: "movie.exe ", Length: 1},
		{Name: "a\nb", Length: 1},
		multiInfo(".ssh", []string{"authorized_keys"}),
		multiInfo("ok", []string{"..", "x"}),
		multiInfo("ok", []string{"sub", "x:y"}),
		multiInfo("ok", []string{""}),
	}
	if runtime.GOOS == "windows" {
		bad = append(bad, &metainfo.Info{Name: "CON", Length: 1}, multiInfo("ok", []string{"nul"}))
	}
	for _, info := range bad {
		if p, err := torrentPaths(dir, info); err == nil {
			t.Errorf("%q %v accepted: %v", info.Name, info.Files, p)
		}
	}
	good := multiInfo("Show S01", []string{"Extras", ".nfo-folder", "a.mkv"}, []string{"b.mkv"})
	p, err := torrentPaths(dir, good)
	if err != nil || len(p) != 2 || p[1] != filepath.Join(dir, "Show S01", "b.mkv") {
		t.Errorf("good torrent: %v, %v", p, err)
	}
}

// writePiece writes piece 0 in full and marks it complete. Completing a
// piece syncs every file it covers (see filePiece.syncData), so a one-byte
// write left the other files missing and MarkComplete failed.
func writePiece(t *testing.T, impl storage.TorrentImpl, info *metainfo.Info) error {
	t.Helper()
	piece := info.Piece(0)
	pc := impl.Piece(piece)
	if _, err := pc.WriteAt(bytes.Repeat([]byte("x"), int(piece.Length())), 0); err != nil {
		return err
	}
	return pc.MarkComplete()
}

func hashN(n byte) (h metainfo.Hash) { h[0] = n; return }

// A torrent is blocked if its paths are taken when it's claimed, stays
// blocked across restarts, and isn't blocked on its own files.
func TestClaim(t *testing.T) {
	dir, owned := t.TempDir(), t.TempDir()
	pc := storage.NewMapPieceCompletion()
	open := func(info *metainfo.Info, ih metainfo.Hash, d string, adopt bool) (*fileStore, storage.TorrentImpl) {
		t.Helper()
		st := newFileStore(d, pc, owned, adopt)
		impl, err := st.OpenTorrent(t.Context(), info, ih)
		if err != nil {
			t.Fatal(err)
		}
		// Closed before the temporary folder is deleted: Windows can't
		// delete files that are still open.
		t.Cleanup(func() { impl.Close() })
		return st, impl
	}

	// The user's own file at the torrent's path: blocked, untouched, and
	// still blocked after a restart (the audit's reproduced case).
	os.MkdirAll(filepath.Join(dir, "Music"), 0o755)
	os.WriteFile(filepath.Join(dir, "Music", "song.mp3"), []byte("user's own"), 0o644)
	music := multiInfo("Music", []string{"song.mp3"})
	st, impl := open(music, hashN(1), dir, false)
	if st.Blocked() == nil {
		t.Fatal("not blocked over an existing file")
	}
	writePiece(t, impl, music)
	if st, _ = open(music, hashN(1), dir, false); st.Blocked() == nil {
		t.Error("blocked torrent unblocked by a restart")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Music", "song.mp3")); string(b) != "user's own" {
		t.Errorf("existing file changed: %q", b)
	}
	if len(st.files()) != 0 {
		t.Error("a blocked torrent lists files as its own")
	}

	// A torrent that passed isn't blocked on its own files after a restart.
	iso := multiInfo("Linux", []string{"a.iso"})
	st, impl = open(iso, hashN(2), dir, false)
	if st.Blocked() != nil || writePiece(t, impl, iso) != nil {
		t.Fatalf("new torrent: %v", st.Blocked())
	}
	if st, _ = open(iso, hashN(2), dir, false); st.Blocked() != nil {
		t.Fatalf("own file refused after restart: %v", st.Blocked())
	}

	// In another folder, existing files there aren't its own.
	other := t.TempDir()
	os.MkdirAll(filepath.Join(other, "Linux"), 0o755)
	os.WriteFile(filepath.Join(other, "Linux", "a.iso"), []byte("someone else's"), 0o644)
	if st, _ := open(iso, hashN(2), other, false); st.Blocked() == nil {
		t.Error("files in another folder taken as the torrent's own")
	}

	// Saved by an older version (no record): its files are adopted.
	legacy := multiInfo("Old", []string{"old.mkv"})
	os.MkdirAll(filepath.Join(dir, "Old"), 0o755)
	os.WriteFile(filepath.Join(dir, "Old", "old.mkv"), []byte("1"), 0o644)
	if st, _ := open(legacy, hashN(7), dir, false); st.Blocked() == nil {
		t.Error("files adopted for a torrent not from the saved list")
	}
	os.Remove(recordPath(owned, hashN(7), dir))
	if st, _ := open(legacy, hashN(7), dir, true); st.Blocked() != nil || len(st.files()) != 1 {
		t.Errorf("older version's torrent not adopted: %v %v", st.Blocked(), st.files())
	}
}

// Remove never removes a symlink (or junction) the user made, even when it
// has the torrent's folder name.
func TestRemoveKeepsLinks(t *testing.T) {
	dir, target := t.TempDir(), t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "Music")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	f := filepath.Join(dir, "Music", "song.mp3")
	os.WriteFile(f, []byte("x"), 0o644)
	if err := removeFiles(dir, []string{f}, nil, true); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "Music")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the user's link was removed: %v", err)
	}
}

// Remove with files deletes the torrent's own files and its empty folders,
// never other files in a shared folder, and never the download folder.
func TestRemoveFilesOnlyOwn(t *testing.T) {
	dir := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
		return p
	}
	own := []string{mk("Season 1/e01.mkv"), mk("Season 1/Subs/e01.srt")}
	other := mk("Season 1/e02.mkv") // another torrent's file in the same folder
	mk("Season 1.part")             // unrelated
	if err := removeFiles(dir, own, nil, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range own {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s not deleted", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Season 1", "Subs")); !os.IsNotExist(err) {
		t.Error("empty folder left behind")
	}
	for _, p := range []string{other, filepath.Join(dir, "Season 1.part"), dir} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was deleted", p)
		}
	}
}

// A new torrent whose folder inside the download folder is a link (to the
// user's library elsewhere) is refused: it would write there.
func TestClaimRefusesLinkedFolder(t *testing.T) {
	dir, owned, library := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Symlink(library, filepath.Join(dir, "Music")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	st := newFileStore(dir, storage.NewMapPieceCompletion(), owned, false)
	impl, err := st.OpenTorrent(t.Context(), multiInfo("Music", []string{"new.mp3"}), hashN(8))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { impl.Close() })
	if st.Blocked() == nil {
		t.Error("torrent allowed to write through a linked folder")
	}
}

// A piece that's complete and correct in memory when the torrent closes is
// written and recorded, not thrown away (on a slow NAS a backlog of them
// built up, and quitting lost them); one that fails its hash isn't.
func TestCloseWritesFinishedPieces(t *testing.T) {
	dir := t.TempDir()
	pc := storage.NewMapPieceCompletion()
	good := bytes.Repeat([]byte("g"), 16<<10)
	sum := sha1.Sum(good)
	info := &metainfo.Info{Name: "f.bin", PieceLength: 16 << 10, Length: 32 << 10, Pieces: append(sum[:], make([]byte, 20)...)}
	st := newFileStore(dir, pc, "", false)
	impl, err := st.OpenTorrent(t.Context(), info, hashN(9))
	if err != nil {
		t.Fatal(err)
	}
	// A first write creates the file; then each piece is only buffered.
	if _, err := impl.Piece(info.Piece(0)).WriteAt(good, 0); err != nil {
		t.Fatal(err)
	}
	impl.Piece(info.Piece(1)).WriteAt(bytes.Repeat([]byte("b"), 16<<10), 0) // wrong data for piece 1's hash
	if err := impl.Close(); err != nil {
		t.Fatal(err)
	}
	c0, _ := pc.Get(metainfo.PieceKey{InfoHash: hashN(9), Index: 0})
	c1, _ := pc.Get(metainfo.PieceKey{InfoHash: hashN(9), Index: 1})
	if !c0.Complete || c1.Complete {
		t.Fatalf("piece 0 recorded %v (want true), piece 1 %v (want false)", c0.Complete, c1.Complete)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "f.bin")); !bytes.Equal(b[:16<<10], good) {
		t.Error("piece 0's data wasn't written")
	}
}

// openStore opens info in dir with ownership records in owned, closing it
// when the test ends.
func openStore(t *testing.T, dir, owned string, info *metainfo.Info, ih metainfo.Hash) (*fileStore, storage.TorrentImpl) {
	t.Helper()
	st := newFileStore(dir, storage.NewMapPieceCompletion(), owned, false)
	impl, err := st.OpenTorrent(t.Context(), info, ih)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { impl.Close() })
	return st, impl
}

// A file the torrent created that's since been replaced (deleted, and a new
// one made at its path: the user's, or a reused inode) isn't its own: the
// torrent is blocked over it and never lists it for deleting.
func TestReplacedFileNotOwned(t *testing.T) {
	dir, owned := t.TempDir(), t.TempDir()
	info := multiInfo("Linux", []string{"a.iso"})
	st, impl := openStore(t, dir, owned, info, hashN(3))
	if err := writePiece(t, impl, info); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "Linux", "a.iso")
	if f := st.files(); len(f) != 1 || f[0] != p {
		t.Fatalf("own file not listed: %v", f)
	}
	impl.Close()
	os.Remove(p)
	time.Sleep(10 * time.Millisecond) // creation times are at least this fine everywhere tested
	os.WriteFile(p, []byte("user's"), 0o644)
	st, _ = openStore(t, dir, owned, info, hashN(3))
	if st.Blocked() == nil {
		t.Error("replaced file taken as the torrent's own")
	}
	o := loadOwnership(owned, hashN(3), dir, false)
	if len(o.files()) != 0 {
		t.Errorf("replaced file still recorded: %v", o.files())
	}
}

// A file that appears at one of the torrent's paths after it was checked is
// never written into.
func TestNoWriteIntoLaterFile(t *testing.T) {
	dir, owned := t.TempDir(), t.TempDir()
	info := multiInfo("Show", []string{"a"}, []string{"b"})
	_, impl := openStore(t, dir, owned, info, hashN(4))
	b := filepath.Join(dir, "Show", "b")
	os.MkdirAll(filepath.Dir(b), 0o755)
	os.WriteFile(b, []byte("user's"), 0o644)
	pc := impl.Piece(info.Piece(0))
	pc.WriteAt([]byte("xy"), 0)
	if pc.MarkComplete() == nil {
		t.Error("wrote into a file it didn't create")
	}
	if got, _ := os.ReadFile(b); string(got) != "user's" {
		t.Errorf("file changed: %q", got)
	}
}

// Records are per folder: the same torrent in another folder starts clean,
// and its files in the first folder stay its own there.
func TestOwnershipPerFolder(t *testing.T) {
	a, b, owned := t.TempDir(), t.TempDir(), t.TempDir()
	info := multiInfo("Linux", []string{"a.iso"})
	_, impl := openStore(t, a, owned, info, hashN(5))
	writePiece(t, impl, info)
	impl.Close()
	st, impl := openStore(t, b, owned, info, hashN(5))
	if st.Blocked() != nil || writePiece(t, impl, info) != nil {
		t.Fatalf("second folder: %v", st.Blocked())
	}
	impl.Close()
	for _, d := range []string{a, b} {
		if st, _ := openStore(t, d, owned, info, hashN(5)); st.Blocked() != nil || len(st.files()) != 1 {
			t.Errorf("%s: %v %v", d, st.Blocked(), st.files())
		}
	}
}

// A file noted as pending (the app stopped between creating and recording
// it) is the torrent's own if it starts with the tag noted, or is empty; not
// if the note was cancelled (the path was taken), or if it has the right
// length but other data (a copy of the same file put there).
func TestPendingRecovered(t *testing.T) {
	dir, owned := t.TempDir(), t.TempDir()
	path := func(name string) string { return filepath.Join(dir, name) }
	o := loadOwnership(owned, hashN(6), dir, true)
	tag := newTag(100)
	for _, name := range []string{"tagged", "empty", "taken", "copy"} {
		if o.pending(path(name), tag) != nil {
			t.Fatal("pending")
		}
	}
	o.cancel(path("taken"))
	tagged := make([]byte, 100)
	copy(tagged, tag)
	os.WriteFile(path("tagged"), tagged, 0o644)
	os.WriteFile(path("empty"), nil, 0o644)
	os.WriteFile(path("taken"), tagged, 0o644)
	os.WriteFile(path("copy"), bytes.Repeat([]byte("x"), 100), 0o644)
	o = loadOwnership(owned, hashN(6), dir, true)
	for name, want := range map[string]bool{"tagged": true, "empty": true, "taken": false, "copy": false} {
		if o.has(path(name)) != want {
			t.Errorf("%s: owned %v", name, !want)
		}
	}
	// Recorded for good once loaded; the notes are gone.
	if o = loadOwnership(owned, hashN(6), dir, true); !o.has(path("tagged")) {
		t.Error("recovered file forgotten")
	}
	if b, _ := os.ReadFile(o.path); bytes.Contains(b, []byte("p\t")) {
		t.Errorf("pending notes kept:\n%s", b)
	}
}

// A file the torrent made, deleted, and made again (the app stopping before
// recording the new one) is recovered: the old file's entry, before the
// note, doesn't settle it.
func TestPendingAfterOldEntry(t *testing.T) {
	dir, owned := t.TempDir(), t.TempDir()
	p := filepath.Join(dir, "a.iso")
	os.WriteFile(p, []byte("old"), 0o644)
	o := loadOwnership(owned, hashN(13), dir, true)
	if err := o.add(p); err != nil {
		t.Fatal(err)
	}
	os.Remove(p)
	time.Sleep(10 * time.Millisecond)
	tag := newTag(50)
	o.pending(p, tag)
	os.WriteFile(p, append(tag, make([]byte, 34)...), 0o644)
	if !loadOwnership(owned, hashN(13), dir, true).has(p) {
		t.Error("re-created file not recovered")
	}
}

// The store's new files start with the tag noted before creating them, at
// their full length, and are then recorded.
func TestNewFileTagged(t *testing.T) {
	dir, owned := t.TempDir(), t.TempDir()
	info := multiInfo("T", []string{"a"})
	info.Files[0].Length = 40
	st, _ := openStore(t, dir, owned, info, hashN(14))
	f, err := st.ft.createFile(0)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	p := filepath.Join(dir, "T", "a")
	b, _ := os.ReadFile(p)
	rec, _ := os.ReadFile(recordPath(owned, hashN(14), dir))
	var tag string
	for _, line := range strings.Split(string(rec), "\n") {
		if strings.HasPrefix(line, "p\t") {
			tag = strings.Split(line, "\t")[1]
		}
	}
	if len(b) != 40 || bytes.Equal(b[:16], make([]byte, 16)) || !hasTag(p, tag) {
		t.Errorf("file %x, note %q", b, tag)
	}
	if !st.ownership().has(p) {
		t.Error("not recorded")
	}
}

// A file two torrents' records both claim (v0.2.36 let two torrents share
// one) is reported, so removing one doesn't delete the other's file.
func TestOwnedByOthers(t *testing.T) {
	dir, owned := t.TempDir(), t.TempDir()
	p := filepath.Join(dir, "same.iso")
	os.WriteFile(p, []byte("1"), 0o644)
	for _, ih := range []metainfo.Hash{hashN(9), hashN(10)} {
		o := loadOwnership(owned, ih, dir, true)
		if err := o.adopt([]string{p}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !ownedByOthers(owned, hashN(9), []string{p})[p] {
		t.Error("shared file not reported")
	}
	loadOwnership(owned, hashN(10), dir, true).forget()
	if ownedByOthers(owned, hashN(9), []string{p})[p] {
		t.Error("file reported shared after the other torrent was removed")
	}
}

// Removing deletes the folders the torrent created, not ones that were
// already there (even empty ones named like it).
func TestRemovePrunesOnlyCreatedFolders(t *testing.T) {
	dir, owned := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(dir, "Show"), 0o755) // the user's, empty
	info := multiInfo("Show", []string{"Season 1", "e01.mkv"})
	st, impl := openStore(t, dir, owned, info, hashN(11))
	if err := writePiece(t, impl, info); err != nil {
		t.Fatal(err)
	}
	impl.Close()
	o := st.ownership()
	if err := removeFiles(dir, o.files(), o.folders(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Show", "Season 1")); !os.IsNotExist(err) {
		t.Error("created folder left behind")
	}
	if _, err := os.Stat(filepath.Join(dir, "Show")); err != nil {
		t.Error("the user's folder was removed")
	}
}

// A torrent that passed v0.2.37's check (an empty marker at the record's
// path) keeps its files after upgrading; one without a marker doesn't get
// them.
func TestV0237MarkerAdopted(t *testing.T) {
	dir, owned := t.TempDir(), t.TempDir()
	p := filepath.Join(dir, "Linux", "a.iso")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("x"), 0o644)
	info := multiInfo("Linux", []string{"a.iso"})
	if st, _ := openStore(t, dir, owned, info, hashN(12)); st.Blocked() == nil {
		t.Fatal("existing file taken without a marker")
	}
	os.Remove(recordPath(owned, hashN(12), dir))
	os.WriteFile(recordPath(owned, hashN(12), dir), nil, 0o600)
	st, _ := openStore(t, dir, owned, info, hashN(12))
	if st.Blocked() != nil || len(st.files()) != 1 {
		t.Fatalf("v0.2.37 torrent not adopted: %v %v", st.Blocked(), st.files())
	}
	if o := loadOwnership(owned, hashN(12), dir, false); !o.written || !o.has(p) {
		t.Error("adoption not recorded")
	}
}

// The same folder written another way (a link, as D:\dl and d:\dl or a
// share's drive letter and \\server path are on Windows) still counts as
// sharing the file.
func TestOwnedByOthersOtherSpelling(t *testing.T) {
	dir, owned := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	p := filepath.Join(dir, "same.iso")
	os.WriteFile(p, []byte("1"), 0o644)
	if err := loadOwnership(owned, hashN(15), alias, true).adopt([]string{filepath.Join(alias, "same.iso")}, nil); err != nil {
		t.Fatal(err)
	}
	if !ownedByOthers(owned, hashN(16), []string{p})[p] {
		t.Error("file shared under another spelling of the folder not reported")
	}
}

// A torrent whose download folder is unavailable when it opens (its drive
// disconnected) isn't claimed, keeps its ownership record and its recorded
// progress, refuses writes, and is claimed, with its own files, once the
// folder is back.
func TestFolderUnavailableAtOpen(t *testing.T) {
	base, owned := t.TempDir(), t.TempDir()
	dir := filepath.Join(base, "Plex")
	os.Mkdir(dir, 0o755)
	info := multiInfo("Linux", []string{"a.iso"})
	pc := storage.NewMapPieceCompletion()
	open := func() (*fileStore, storage.TorrentImpl) {
		st := newFileStore(dir, pc, owned, false)
		impl, err := st.OpenTorrent(t.Context(), info, hashN(17))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { impl.Close() })
		return st, impl
	}
	_, impl := open()
	if err := writePiece(t, impl, info); err != nil {
		t.Fatal(err)
	}
	impl.Close()
	record, _ := os.ReadFile(recordPath(owned, hashN(17), dir))

	away := filepath.Join(base, "away")
	os.Rename(dir, away) // the drive is disconnected
	st, impl := open()
	if st.Unavailable() == nil || st.Blocked() != nil {
		t.Fatalf("unavailable %v, blocked %v", st.Unavailable(), st.Blocked())
	}
	if c := impl.Piece(info.Piece(0)).Completion(); !c.Ok || !c.Complete {
		t.Errorf("recorded progress lost while unavailable: %+v", c)
	}
	if b, _ := os.ReadFile(recordPath(owned, hashN(17), dir)); !bytes.Equal(b, record) {
		t.Errorf("ownership record changed while unavailable:\n%s\nwas\n%s", b, record)
	}
	if writePiece(t, impl, info) == nil {
		t.Error("wrote while the folder was unavailable")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the download folder was created")
	}

	os.Rename(away, dir) // back
	if err := st.reclaim(); err != nil || st.Unavailable() != nil || st.Blocked() != nil {
		t.Fatalf("after it's back: %v, unavailable %v, blocked %v", err, st.Unavailable(), st.Blocked())
	}
	if f := st.files(); len(f) != 1 {
		t.Errorf("own file not recognised after reclaim: %v", f)
	}
	if writePiece(t, impl, info) != nil {
		t.Error("can't write once the folder is back")
	}
}

// A torrent running when its folder goes away notices on the next file it
// can't open, and refuses writes from then on.
func TestFolderGoneWhileRunning(t *testing.T) {
	base, owned := t.TempDir(), t.TempDir()
	dir := filepath.Join(base, "Plex")
	os.Mkdir(dir, 0o755)
	info := multiInfo("Show", []string{"a"}, []string{"b"})
	st, impl := openStore(t, dir, owned, info, hashN(18))
	os.Rename(dir, filepath.Join(base, "away"))
	if writePiece(t, impl, info) == nil {
		t.Error("wrote with the folder gone")
	}
	if st.Unavailable() == nil {
		t.Error("the folder going away wasn't noticed")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the download folder was created")
	}
}

// setMount points the torrent's record at mount as the drive's mount point,
// as claim notes it (tests can't mount anything, so a plain folder stands
// for a drive that's away: nothing is mounted there).
func setMount(t *testing.T, owned string, ih metainfo.Hash, dir, mount string) {
	t.Helper()
	o := loadOwnership(owned, ih, dir, false)
	o.mu.Lock()
	o.mount = mount
	err := o.saveLocked()
	o.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

// A drive mounted without an /etc/fstab line leaves an ordinary folder when
// it's away. The record notes the mount point; while nothing is mounted
// there the folder is away: nothing is claimed, created or tidied.
func TestFolderAwayUnlistedMount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mount points aren't noted on Windows: a drive that's away there has no folder at all")
	}
	dir, owned := t.TempDir(), t.TempDir()
	info := multiInfo("Show", []string{"a"}, []string{"b"})
	_, impl := openStore(t, dir, owned, info, hashN(19))
	if err := writePiece(t, impl, info); err != nil {
		t.Fatal(err)
	}
	impl.Close()
	rec := recordPath(owned, hashN(19), dir)
	if m := recordedMount(rec); m == "" || !isMountRoot(m) {
		t.Fatalf("mount point noted: %q", m)
	}

	setMount(t, owned, hashN(19), dir, dir) // dir is no mount point: "away"
	before, _ := os.ReadFile(rec)
	st, impl := openStore(t, dir, owned, info, hashN(19))
	if st.Unavailable() == nil {
		t.Fatal("opened as available with nothing mounted at its mount point")
	}
	if writePiece(t, impl, info) == nil {
		t.Error("wrote with the drive away")
	}
	if after, _ := os.ReadFile(rec); !bytes.Equal(after, before) {
		t.Errorf("record changed while away:\n%s", after)
	}
}

// Deleting the torrent's files doesn't make the folder look like a drive
// that's away (the folder is still where it was): the torrent re-downloads.
func TestDeletedFilesNotAway(t *testing.T) {
	dir, owned := t.TempDir(), t.TempDir()
	info := multiInfo("Linux", []string{"a.iso"})
	_, impl := openStore(t, dir, owned, info, hashN(22))
	writePiece(t, impl, info)
	impl.Close()
	os.RemoveAll(filepath.Join(dir, "Linux")) // the user deletes them
	st, impl := openStore(t, dir, owned, info, hashN(22))
	if st.Unavailable() != nil || st.Blocked() != nil {
		t.Fatalf("unavailable %v, blocked %v", st.Unavailable(), st.Blocked())
	}
	if writePiece(t, impl, info) != nil {
		t.Error("can't download again after the files were deleted")
	}
}

// A record from before the mount point was noted, whose files are all
// missing at once (its drive away, in a way folderAway can't see), isn't
// tidied: when the drive is back its files are still the torrent's.
func TestAllMissingNotTidied(t *testing.T) {
	base, owned := t.TempDir(), t.TempDir()
	dir := filepath.Join(base, "Plex")
	os.Mkdir(dir, 0o755)
	info := multiInfo("Show", []string{"a"}, []string{"b"})
	_, impl := openStore(t, dir, owned, info, hashN(23))
	writePiece(t, impl, info)
	impl.Close()
	setMount(t, owned, hashN(23), dir, "") // an older record
	rec := recordPath(owned, hashN(23), dir)
	before, _ := os.ReadFile(rec)
	os.Rename(dir, filepath.Join(base, "drive"))
	os.Mkdir(dir, 0o755) // the empty mount point
	loadOwnership(owned, hashN(23), dir, true)
	if after, _ := os.ReadFile(rec); !bytes.Equal(after, before) {
		t.Errorf("record tidied with every file missing:\n%s", after)
	}
	os.Remove(dir)
	os.Rename(filepath.Join(base, "drive"), dir)
	if o := loadOwnership(owned, hashN(23), dir, true); len(o.files()) != 2 {
		t.Errorf("own files not recognised when the drive is back: %v", o.files())
	}
}

// A drive that goes away mid-run: the next file isn't created on the empty
// folder left behind.
func TestNoCreateAfterDriveAway(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mount points aren't noted on Windows: a drive that's away there has no folder at all")
	}
	dir, owned := t.TempDir(), t.TempDir()
	info := multiInfo("Show", []string{"a"}, []string{"b"})
	info.Files[0].Length, info.Files[1].Length = 8, 8
	st, _ := openStore(t, dir, owned, info, hashN(24))
	f, err := st.ft.createFile(0)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	st.mu.Lock()
	st.mount = dir // the drive went away: nothing mounted there now
	st.mu.Unlock()
	if f, err := st.ft.createFile(1); err == nil {
		f.Close()
		t.Error("created a file with the drive away")
	}
	if _, err := os.Stat(filepath.Join(dir, "Show", "b")); !os.IsNotExist(err) {
		t.Error("file made on the empty folder")
	}
	if st.Unavailable() == nil {
		t.Error("not noticed")
	}
}

// A piece finished before the drive dropped mid-run stays finished: the
// failed stat checks the folder first.
func TestCompletionAfterDriveDrop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows won't rename a folder with files open in it, which is how the test takes the drive away")
	}
	base, owned := t.TempDir(), t.TempDir()
	dir := filepath.Join(base, "Plex")
	os.Mkdir(dir, 0o755)
	info := multiInfo("Linux", []string{"a.iso"})
	st, impl := openStore(t, dir, owned, info, hashN(21))
	if err := writePiece(t, impl, info); err != nil {
		t.Fatal(err)
	}
	os.Rename(dir, filepath.Join(base, "away"))
	if c := impl.Piece(info.Piece(0)).Completion(); !c.Ok || !c.Complete {
		t.Errorf("finished piece reported missing after the drive dropped: %+v", c)
	}
	if st.Unavailable() == nil {
		t.Error("the drop wasn't noticed")
	}
}

// A record without a mount point (from v0.2.38, say) whose files are all
// missing at once waits: claiming it would note this computer's disk as the
// drive, download onto it, then refuse the real files once the drive is
// back. When they're back it's claimed with its own files.
func TestLegacyAllMissingWaits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the all-missing rule is off on Windows, where a drive that's away has no folder at all")
	}
	base, owned := t.TempDir(), t.TempDir()
	dir := filepath.Join(base, "Plex")
	os.Mkdir(dir, 0o755)
	info := multiInfo("Show", []string{"a"}, []string{"b"})
	_, impl := openStore(t, dir, owned, info, hashN(25))
	writePiece(t, impl, info)
	impl.Close()
	setMount(t, owned, hashN(25), dir, "") // an older record
	rec := recordPath(owned, hashN(25), dir)
	before, _ := os.ReadFile(rec)
	os.Rename(dir, filepath.Join(base, "drive"))
	os.Mkdir(dir, 0o755) // the empty mount point
	st, impl := openStore(t, dir, owned, info, hashN(25))
	if st.Unavailable() == nil {
		t.Fatal("claimed with every file missing")
	}
	if writePiece(t, impl, info) == nil {
		t.Error("wrote onto the empty mount point")
	}
	if after, _ := os.ReadFile(rec); !bytes.Equal(after, before) {
		t.Errorf("record changed while waiting:\n%s", after)
	}
	os.Remove(dir)
	os.Rename(filepath.Join(base, "drive"), dir)
	if st.unavailableNow() != nil {
		t.Fatal("still waiting with the files back")
	}
	if err := st.reclaim(); err != nil || len(st.files()) != 2 {
		t.Fatalf("reclaim: %v, files %v", err, st.files())
	}
	if recordedMount(rec) == "" {
		t.Error("mount point not noted once the drive was there")
	}
}

// A download folder reached through a link is judged by where the link
// points: the link's own folder says nothing about the drive.
func TestMountRootThroughLink(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "Media")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if got, want := mountRoot(link), mountRoot(target); got != want || got == link {
		t.Errorf("mountRoot(link) = %q, want %q", got, want)
	}
}
