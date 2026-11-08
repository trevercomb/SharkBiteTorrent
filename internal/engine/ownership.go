package engine

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	g "github.com/anacrolix/generics"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// ownership records which of a torrent's files this app created in one
// download folder, so the store only ever writes into and deletes its own
// files: never a file that was there before (the user's, or another
// torrent's with the same name), and never one that appeared at the same
// path later.
//
// A file is recorded when the store creates it (O_EXCL), with its identity
// (see fileIdentity: creation time, plus the file index on Windows). It
// counts as ours only while the file at that path still is that file;
// entries that no longer match are dropped. The folders the store created
// are recorded too, so Remove prunes only those. Before creating a file the
// store notes it as pending (with its length), so a crash between creating
// and recording it doesn't leave the torrent blocked on its own file.
//
// Records are owned/<info hash>-<folder hash> in the app's data folder,
// appended as files are created. Torrents saved by v0.2.36 or older have
// none; their existing files are adopted once (fileStore.adopt, decided
// when the saved list is loaded). So are those of torrents that passed
// v0.2.37's check, which left an empty marker file at the record's path.
type ownership struct {
	path string // the record file
	dir  string // download folder

	mu      sync.Mutex
	owned   map[string]fileIdentity // files, by absolute path
	dirs    map[string]bool         // folders the store created
	written bool                    // the record file exists with its header
	marker  bool                    // instead, an empty v0.2.37 marker: the torrent passed its check then
	mount   string                  // the mount point the download folder was on (see folderAway); "" if not known
}

// folderAway is folderUnavailable plus what the torrent's record knows. A
// drive mounted without an /etc/fstab line (a systemd mount unit, autofs,
// by hand) leaves an ordinary folder behind when it's away, which
// folderUnavailable can't tell from the drive; using it would tidy the
// record of every file and download onto this computer's own disk. So the
// record notes the mount point the folder was on (the NAS's /mnt/nas, say)
// and the drive is away while nothing is mounted there. Not the device
// number (a network share gets a new one at every mount), the filesystem
// type (one disk can be mounted by different drivers) or an empty folder
// (the user may have deleted the files).
func folderAway(dir, mount string) error {
	if err := folderUnavailable(dir); err != nil {
		return err
	}
	if mount != "" && !isMountRoot(mount) {
		return fmt.Errorf("the download folder %s isn't available: the drive that was mounted at %s isn't now (is it connected?). The torrent continues when it's back", dir, mount)
	}
	return nil
}

// allListedMissing reports whether a record lists files and none of them
// is there (see fileStore.unavailableNow).
func allListedMissing(record, dir string) bool {
	b, err := os.ReadFile(record)
	if err != nil || !bytes.HasPrefix(b, []byte(ownershipHeader)) {
		return false
	}
	listed := false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 || f[0] != "f" {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(f[2]))
		if !isSub(dir, p) {
			continue
		}
		listed = true
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return listed
}

// recordedMount reads the mount point a record noted, without checking
// anything.
func recordedMount(record string) string {
	b, err := os.ReadFile(record)
	if err != nil || !bytes.HasPrefix(b, []byte(ownershipHeader)) {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.SplitN(line, "\t", 3); len(f) == 3 && f[0] == "m" {
			if m, err := hex.DecodeString(f[1]); err == nil {
				return string(m)
			}
		}
	}
	return ""
}

const ownershipHeader = "sbt-owned 3\t"

func recordPath(ownedDir string, ih metainfo.Hash, dir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	return filepath.Join(ownedDir, ih.HexString()+"-"+hex.EncodeToString(sum[:8]))
}

// loadOwnership reads the torrent's record for dir, keeping only entries
// whose file is still the one recorded (and completing pending ones whose
// file was created after they were noted). With tidy, the record is
// rewritten without the entries dropped; only while nothing else is writing
// to it (never another torrent's, which may be downloading).
func loadOwnership(ownedDir string, ih metainfo.Hash, dir string, tidy bool) *ownership {
	o := &ownership{path: recordPath(ownedDir, ih, dir), dir: filepath.Clean(dir), owned: map[string]fileIdentity{}, dirs: map[string]bool{}}
	b, err := os.ReadFile(o.path)
	if err != nil {
		return o
	}
	if len(b) == 0 {
		o.marker = true
		return o
	}
	i := bytes.LastIndexByte(b, '\n')
	if i < 0 {
		return o
	}
	b = b[:i+1] // a last line without its newline was cut off by a crash
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), ownershipHeader) || filepath.Clean(strings.TrimPrefix(sc.Text(), ownershipHeader)) != o.dir {
		return o // not a record for this folder (or an older format: not trusted)
	}
	o.written = true
	// With its drive disconnected every file would look gone: keep the
	// entries as recorded (has checks each file when it's used) and leave
	// the record alone.
	available := folderAway(o.dir, recordedMount(o.path)) == nil
	if !available {
		tidy = false
	}
	rewrite := false
	pending := map[string]string{}      // noted (with the file's tag), and neither recorded nor cancelled since
	listed := map[string]fileIdentity{} // every "f" line, current or not
	allMissing := true
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), "\t", 3)
		if len(f) != 3 {
			rewrite = true
			continue
		}
		p := filepath.Join(o.dir, filepath.FromSlash(f[2]))
		if !isSub(o.dir, p) || p == o.dir {
			rewrite = true
			continue
		}
		switch f[0] {
		case "f":
			delete(pending, p) // recorded after the note (one before it doesn't settle it)
			want, err := parseIdentity(f[1])
			if err == nil {
				listed[p] = want
			}
			if err == nil && !available {
				o.owned[p] = want
				continue
			}
			if _, lerr := os.Lstat(p); !errors.Is(lerr, fs.ErrNotExist) {
				allMissing = false
			}
			if got, gerr := identityOf(p); err != nil || gerr != nil || !got.same(want) {
				rewrite = true // gone, or a different file now
				continue
			}
			o.owned[p] = want
		case "d":
			o.dirs[p] = true
		case "m":
			if m, err := hex.DecodeString(f[1]); err == nil {
				o.mount = string(m)
			}
		case "p":
			rewrite = true
			pending[p] = f[1]
		case "x":
			rewrite = true
			delete(pending, p)
		default:
			rewrite = true
		}
	}
	if available && len(listed) > 0 && allMissing {
		// Every file it lists is gone at once: more likely a drive that's
		// away in a way folderAway can't see (a record from before the
		// mount point was noted) than files the user deleted. Keeping the
		// entries costs nothing (has checks each file when it's used).
		for p, w := range listed {
			o.owned[p] = w
		}
		tidy = false
	}
	for p, tag := range pending {
		if !available {
			break // judged once the drive is back
		}
		// Noted just before the store created it, and neither recorded nor
		// cancelled after (the app stopped in between): ours if the file
		// there starts with the tag the store wrote into it, or is empty
		// (created, not tagged yet; nothing to lose if it isn't ours). Not
		// by size (a copy of the same file has it) or creation time (a NAS
		// stamps it with its own clock, seconds off this computer's).
		if hasTag(p, tag) {
			if got, err := identityOf(p); err == nil && got.known() {
				o.owned[p] = got
			}
		}
	}
	if rewrite && tidy {
		o.mu.Lock()
		o.saveLocked()
		o.mu.Unlock()
	}
	return o
}

// has reports whether the file at p is one the store created.
func (o *ownership) has(p string) bool {
	o.mu.Lock()
	want, ok := o.owned[p]
	o.mu.Unlock()
	if !ok {
		return false
	}
	got, err := identityOf(p)
	return err == nil && got.same(want)
}

// pending notes that the store is about to create p and write tag (see
// newTag) at its start.
func (o *ownership) pending(p string, tag []byte) error {
	return o.appendLine("p", hex.EncodeToString(tag)+"-", p)
}

// newTag returns the bytes a new file of length n starts with until its
// data is written: random, so a file that starts with them was made by the
// store (see loadOwnership).
func newTag(n int64) []byte {
	tag := make([]byte, min(n, 16))
	rand.Read(tag)
	return tag
}

// hasTag reports whether the regular file at p is empty or starts with the
// tag noted ("<hex>-"; notes without the dash, from v0.2.38, held a length
// and match only an empty file).
func hasTag(p, noted string) bool {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if fi.Size() == 0 {
		return true
	}
	h, ok := strings.CutSuffix(noted, "-")
	tag, err := hex.DecodeString(h)
	if !ok || err != nil || len(tag) == 0 {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	got := make([]byte, len(tag))
	_, err = io.ReadFull(f, got)
	return err == nil && bytes.Equal(got, tag)
}

// cancel withdraws a pending note: the file couldn't be created.
func (o *ownership) cancel(p string) {
	o.appendLine("x", "-", p)
}

// addDir records a folder the store created.
func (o *ownership) addDir(p string) error {
	if err := o.appendLine("d", "-", p); err != nil {
		return err
	}
	o.mu.Lock()
	o.dirs[p] = true
	o.mu.Unlock()
	return nil
}

// add records a file the store has just created.
func (o *ownership) add(p string) error {
	id, err := identityOf(p)
	if err != nil {
		return err
	}
	if !id.known() {
		return fmt.Errorf("%s: this drive doesn't identify files, so Shark BiteTorrent can't tell its own files from others there", filepath.Dir(p))
	}
	if err := o.appendLine("f", id.String(), p); err != nil {
		return err
	}
	o.mu.Lock()
	o.owned[p] = id // only once it's on disk
	o.mu.Unlock()
	return nil
}

// appendLine adds "kind<TAB>value<TAB>path" to the record, writing the whole
// record first if it doesn't exist yet (or was deleted meanwhile).
func (o *ownership) appendLine(kind, value, p string) error {
	rel, err := filepath.Rel(o.dir, p)
	if err != nil {
		return err
	}
	line := kind + "\t" + value + "\t" + filepath.ToSlash(rel) + "\n"
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.written {
		f, err := os.OpenFile(o.path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			_, werr := f.WriteString(line)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			return werr
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := o.saveLocked(); err != nil {
		return err
	}
	f, err := os.OpenFile(o.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(line)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// adopt records existing files, and the folders between them and the
// download folder, as ours: torrents saved by an older version.
//
// A file is adopted only if it looks like the torrent's: the app sizes every
// file it creates to its full length up front, so a file of exactly that
// length, or no longer than it when the torrent has finished pieces recorded
// in it (a WD NAS can report a pre-sized file as only as long as what's been
// written). Anything else, such as a file the user put there later at a path
// an older version skipped, isn't adopted and blocks the torrent instead.
// lengths[i] >= 0: the full length, progress recorded; -(length)-1: no
// progress recorded, exact length only. nil lengths: no check (tests).
func (o *ownership) adopt(paths []string, lengths []int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, p := range paths {
		if i < len(lengths) {
			fi, err := os.Lstat(p)
			if err != nil {
				continue
			}
			if l := lengths[i]; l >= 0 && (fi.Size() > l || fi.Size() == 0 && l > 0) ||
				l < 0 && fi.Size() != -l-1 {
				continue
			}
		}
		if id, err := identityOf(p); err == nil && id.known() {
			o.owned[p] = id
			for d := filepath.Dir(p); d != o.dir && isSub(o.dir, d); d = filepath.Dir(d) {
				o.dirs[d] = true
			}
		}
	}
	return o.saveLocked()
}

// saveLocked rewrites the whole record. Call with o.mu held.
func (o *ownership) saveLocked() error {
	var b strings.Builder
	b.WriteString(ownershipHeader + o.dir + "\n")
	if o.mount != "" {
		b.WriteString("m\t" + hex.EncodeToString([]byte(o.mount)) + "\t-\n")
	}
	for _, p := range sortedKeys(o.dirs) {
		if rel, err := filepath.Rel(o.dir, p); err == nil {
			b.WriteString("d\t-\t" + filepath.ToSlash(rel) + "\n")
		}
	}
	for _, p := range sortedKeys(o.owned) {
		if rel, err := filepath.Rel(o.dir, p); err == nil {
			b.WriteString("f\t" + o.owned[p].String() + "\t" + filepath.ToSlash(rel) + "\n")
		}
	}
	if err := os.MkdirAll(filepath.Dir(o.path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(o.path), filepath.Base(o.path)+".*.tmp")
	if err != nil {
		return err
	}
	_, werr := f.WriteString(b.String())
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(f.Name(), o.path)
	}
	if werr != nil {
		os.Remove(f.Name())
		return werr
	}
	o.written = true
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// files returns the recorded files that are still the ones created.
func (o *ownership) files() []string {
	o.mu.Lock()
	paths := sortedKeys(o.owned)
	o.mu.Unlock()
	out := paths[:0]
	for _, p := range paths {
		if o.has(p) {
			out = append(out, p)
		}
	}
	return out
}

// folders returns the folders the store created, deepest first.
func (o *ownership) folders() []string {
	o.mu.Lock()
	out := sortedKeys(o.dirs)
	o.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func (o *ownership) forget() { os.Remove(o.path) }

// ownedByOthers reports which of paths another torrent's record also
// claims (two torrents sharing a file, as v0.2.36 allowed): such a file
// mustn't be deleted when one of them is removed. Matched by identity, in
// every record, so the same folder written differently (D:\dl and d:\dl,
// or a share's drive letter and its \\server path) still counts.
func ownedByOthers(ownedDir string, self metainfo.Hash, paths []string) map[string]bool {
	ents, err := os.ReadDir(ownedDir)
	if err != nil {
		return nil
	}
	ids := map[string]fileIdentity{}
	for _, p := range paths {
		if id, err := identityOf(p); err == nil && id.known() {
			ids[p] = id
		}
	}
	shared := map[string]bool{}
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, self.HexString()) || strings.HasSuffix(name, ".tmp") {
			continue
		}
		for _, other := range recordedIdentities(filepath.Join(ownedDir, name)) {
			for p, id := range ids {
				if id.same(other) {
					shared[p] = true
				}
			}
		}
	}
	return shared
}

// recordedIdentities returns the identities in a record's "f" lines, without
// checking the files (another torrent's record, perhaps for another folder).
func recordedIdentities(path string) []fileIdentity {
	b, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(b, []byte(ownershipHeader)) {
		return nil
	}
	var out []fileIdentity
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.SplitN(line, "\t", 3); len(f) == 3 && f[0] == "f" {
			if id, err := parseIdentity(f[1]); err == nil && id.known() {
				out = append(out, id)
			}
		}
	}
	return out
}

// claim prepares the torrent's ownership record and checks that none of its
// paths is taken by a file that isn't ours. adopt is for a torrent saved by
// an older version, with no record yet: what's at its paths is its own.
func (s *fileStore) claim(ih metainfo.Hash, info *metainfo.Info, paths []string) error {
	if s.ownedDir == "" {
		return nil // tests and tools: no records
	}
	os.Remove(filepath.Join(s.ownedDir, ih.HexString())) // pre-release marker: not trusted
	o := loadOwnership(s.ownedDir, ih, s.dir, true)
	// Adoption too: a linked folder would let an old torrent claim files
	// wherever the link points.
	if err := noLinkedFolders(s.dir, paths); err != nil {
		return err
	}
	// The mount point the folder is on, for folderAway (claim only runs
	// while the folder is available, so this is the drive's).
	if m := mountRoot(s.dir); m != "" && m != o.mount {
		o.mu.Lock()
		o.mount = m
		if o.written {
			o.saveLocked() // a record from before this was noted
		}
		o.mu.Unlock()
	}
	s.mu.Lock()
	s.mount = o.mount
	s.mu.Unlock()
	switch {
	case !o.written && (s.adopt || o.marker):
		lengths := make([]int64, len(paths))
		var off int64
		for i, f := range info.UpvertedFiles() {
			if i < len(lengths) {
				lengths[i] = f.Length
				if f.Length > 0 && !anyComplete(s.pc, ih, info, off, f.Length) {
					// No progress recorded in it: only an exact-length
					// file counts (see adopt).
					lengths[i] = -f.Length - 1
				}
			}
			off += f.Length
		}
		if err := o.adopt(paths, lengths); err != nil {
			return err
		}
	case !o.written:
		// New here: progress recorded for this torrent in another folder
		// (or before it was removed) doesn't apply to these files.
		clearComplete(s.pc, ih, info.NumPieces())
		// Written now, so "no record" always means "not ours yet", never
		// "added by this version but nothing created so far".
		o.mu.Lock()
		err := o.saveLocked()
		o.mu.Unlock()
		if err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.own = o
	s.mu.Unlock()
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil && !o.has(p) {
			rel, _ := filepath.Rel(s.dir, p)
			return fmt.Errorf("%q already exists in the download folder, and Shark BiteTorrent only writes into files it created. Remove this torrent, move the file away (or choose another download folder), then add the torrent again", rel)
		}
	}
	return nil
}

// noLinkedFolders refuses a torrent whose folders inside dir include a link
// (symlink, or junction on Windows) that's already there: the torrent would
// write into wherever it points, outside the download folder.
func noLinkedFolders(dir string, paths []string) error {
	seen := map[string]bool{}
	for _, p := range paths {
		for d := filepath.Dir(p); d != filepath.Clean(dir) && isSub(dir, d) && !seen[d]; d = filepath.Dir(d) {
			seen[d] = true
			if fi, err := os.Lstat(d); err == nil && !fi.IsDir() {
				rel, _ := filepath.Rel(dir, d)
				return fmt.Errorf("%q in the download folder is a link or a file, not a folder, and the torrent's files would go there. Remove this torrent, move it away (or choose another download folder), then add the torrent again", rel)
			}
		}
	}
	return nil
}

// clearComplete forgets the torrent's finished pieces (only those: each
// write is a disk sync).
func clearComplete(pc storage.PieceCompletion, ih metainfo.Hash, n int) {
	for i := range n {
		key := metainfo.PieceKey{InfoHash: ih, Index: i}
		if c, err := pc.Get(key); err == nil && c.Ok && c.Complete {
			pc.Set(key, g.Some(false))
		}
	}
}

// anyComplete reports whether any piece overlapping [off, off+length) of the
// torrent's data is recorded as finished.
func anyComplete(pc storage.PieceCompletion, ih metainfo.Hash, info *metainfo.Info, off, length int64) bool {
	pl := info.PieceLength
	if pl <= 0 || pc == nil {
		return false
	}
	first, last := int(off/pl), int((off+length-1)/pl)
	for i := first; i <= last && i < info.NumPieces(); i++ {
		if c, err := pc.Get(metainfo.PieceKey{InfoHash: ih, Index: i}); err == nil && c.Ok && c.Complete {
			return true
		}
	}
	return false
}
