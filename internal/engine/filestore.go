package engine

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/segments"
	"github.com/anacrolix/torrent/storage"

	"sharkbite/internal/platform"
	"sharkbite/internal/scan"
)

// fileStore is torrent storage tuned for slow or network disks (e.g. a NAS
// share), where anacrolix's default file storage managed ~0.5 MB/s:
//
//   - Files stay open while the torrent is active, instead of an open/close
//     per 16 KiB block (each one a network round trip on SMB).
//   - New files are sparse and pre-sized, so writing past the end doesn't
//     make the filesystem zero-fill the gap first.
//   - Blocks are gathered in memory per piece and written in one go once the
//     piece passes its hash check. On a NAS, 16 KiB random writes ran at
//     ~2 MB/s; piece-sized writes at ~17 MB/s.
//   - Piece completion is persisted (shared database in the app's data
//     folder), so restarts don't re-hash or re-download anything.
//
// Files are written at their final names. Handles are closed and unfinished
// buffers dropped when the torrent is dropped, so files can be deleted after.
//
// The store only writes into and deletes files it created for this torrent
// in this folder (see ownership.go); a torrent whose paths are taken by
// other files is blocked instead.
type fileStore struct {
	// suspect: why data written for this torrent may not have reached the
	// drive intact (its drive went away, a read or write failed), for an
	// automatic Verify before the malware scan; "" if nothing happened.
	suspect  atomic.Pointer[string]
	dir      string // download folder for this torrent
	pc       storage.PieceCompletion
	ownedDir string // ownership records (see ownership.go); "" skips the checks
	adopt    bool   // a torrent saved by an older version: adopt its files if it has no record yet

	mu      sync.Mutex
	own     *ownership    // set by claim; nil without ownedDir
	blocked error         // set if the torrent mustn't write (shown in the list)
	gone    error         // the download folder isn't available (its drive is disconnected); see reclaim
	ih      metainfo.Hash // the torrent's, once it's being opened
	mount   string        // the mount point its record noted (see folderAway)
	// onMarkFailed: set by the engine, called when a finished piece couldn't
	// be saved (see markFailed).
	onMarkFailed func(idx int)
	ft           *fileTorrent // the torrent's storage, once opened
	removed      bool         // being removed: don't write out buffered pieces on close
}

// dropping tells the store its torrent is being removed, before it's closed.
func (s *fileStore) dropping() {
	s.mu.Lock()
	s.removed = true
	s.mu.Unlock()
}

// files are the torrent's files that it created (none if it's blocked).
// Without ownership records: those that exist, as regular files, at its
// paths.
func (s *fileStore) files() []string {
	s.mu.Lock()
	t, blocked, own := s.ft, s.blocked, s.own
	s.mu.Unlock()
	if t == nil || blocked != nil {
		return nil
	}
	if own != nil {
		return own.files()
	}
	var out []string
	for _, p := range t.paths {
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	return out
}

// folderUnavailable reports why dir can't be used now: it's missing, or
// (Linux) it's on a drive that isn't mounted, whose mount point is an empty
// folder on this computer's own disk.
func folderUnavailable(dir string) error {
	if mp := platform.UnmountedDrive(dir); mp != "" {
		return fmt.Errorf("the download folder %s isn't available: the drive at %s isn't mounted. The torrent continues when it's back", dir, mp)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("the download folder %s isn't available (is its drive connected?). The torrent continues when it's back", dir)
	}
	return nil
}

// Unavailable reports why the torrent's download folder can't be used, if
// it can't (shown in the list until it's back).
func (s *fileStore) Unavailable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gone
}

// unavailableNow checks whether the download folder can be used now (see
// folderAway), with the mount point the torrent's record noted.
func (s *fileStore) unavailableNow() error {
	s.mu.Lock()
	mount, ih := s.mount, s.ih
	s.mu.Unlock()
	if err := folderAway(s.dir, mount); err != nil {
		return err
	}
	// A record from before mount points were noted (or never claimed
	// while its drive was there): if every file it lists is missing at
	// once, its drive is more likely away than the files deleted. Claiming
	// it now would note this computer's disk as the drive, download onto
	// it, and then refuse the real files once the drive is back. So it
	// waits for a file to reappear. (Not on Windows, which notes no mount
	// points: a drive that's away there has no folder at all.)
	if mount == "" && s.ownedDir != "" && ih != (metainfo.Hash{}) && mountRoot("/") != "" &&
		allListedMissing(recordPath(s.ownedDir, ih, s.dir), s.dir) {
		return fmt.Errorf("none of the torrent's files are in %s (is its drive connected?). The torrent continues when they're back; if you deleted them, remove the torrent and add it again", s.dir)
	}
	return nil
}

// checkGone notes that the download folder has become unavailable, if it
// has (after a file couldn't be opened, written or found), and reports
// whether.
func (s *fileStore) checkGone() bool {
	err := s.unavailableNow()
	if err == nil {
		return false
	}
	s.mu.Lock()
	if s.gone == nil {
		s.gone = err
	}
	s.mu.Unlock()
	// Over any earlier reason: a write that failed as the drive dropped is
	// recorded first, but "disconnected" says what happened.
	why := "its drive was disconnected"
	s.suspect.Store(&why)
	return true
}

// markSuspect notes why the torrent's data needs checking (the first
// reason wins); Suspect reports it, ClearSuspect forgets it.
func (s *fileStore) markSuspect(why string) { s.suspect.CompareAndSwap(nil, &why) }

func (s *fileStore) Suspect() string {
	if p := s.suspect.Load(); p != nil {
		return *p
	}
	return ""
}

func (s *fileStore) ClearSuspect() { s.suspect.Store(nil) }

// reclaim runs once the download folder is back: the torrent's check
// (claim), skipped while its files couldn't be seen, and then writing
// resumes. A claim that fails blocks the torrent as usual.
func (s *fileStore) reclaim() error {
	s.mu.Lock()
	t, claimed := s.ft, s.own != nil
	s.mu.Unlock()
	var err error
	if t != nil && !claimed && s.Blocked() == nil {
		if err = s.claim(t.ih, t.info, t.paths); err != nil {
			s.block(err)
		}
		t.mu.Lock()
		if !t.closed { // after close, writeFinished reads own and blocked unlocked
			t.own, t.blocked = s.ownership(), err
		}
		t.mu.Unlock()
	}
	if t != nil {
		t.dropHandles() // opened over the old connection: they'd fail every write
	}
	s.mu.Lock()
	s.gone = nil
	s.mu.Unlock()
	return err
}

// ownership is the torrent's record of its files, once claimed.
func (s *fileStore) ownership() *ownership {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.own
}

func newFileStore(dir string, pc storage.PieceCompletion, ownedDir string, adopt bool) *fileStore {
	return &fileStore{dir: dir, pc: pc, ownedDir: ownedDir, adopt: adopt}
}

// Blocked reports why the torrent may not write its files, if it may not.
func (s *fileStore) Blocked() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked
}

func (s *fileStore) block(err error) {
	s.mu.Lock()
	s.blocked = err
	s.mu.Unlock()
}

// torrentPaths returns where each of the torrent's files goes under dir.
// It refuses names that could leave dir, hide among the user's own files
// (a top-level ".ssh" or ".bashrc" in a home folder), name the download
// folder itself ("" or "."), or mean something special to the OS (Windows
// device names like CON, ":" streams, trailing dots and spaces).
func torrentPaths(dir string, info *metainfo.Info) ([]string, error) {
	name := info.BestName()
	if !safeName(name) || strings.HasPrefix(name, ".") {
		return nil, fmt.Errorf("the torrent's name %q isn't safe to use as a file name", name)
	}
	var out []string
	for _, fi := range info.UpvertedFiles() {
		parts := []string{dir, name}
		if info.IsDir() {
			for _, c := range fi.BestPath() {
				if !safeName(c) {
					return nil, fmt.Errorf("the torrent has a file name that isn't safe: %q", strings.Join(fi.BestPath(), "/"))
				}
				parts = append(parts, c)
			}
		}
		p := filepath.Join(parts...)
		if !isSub(dir, p) || p == filepath.Clean(dir) {
			return nil, fmt.Errorf("unsafe file path in torrent: %q", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// safeName reports whether c is usable as one file or folder name.
func safeName(c string) bool {
	return c != "" && c != "." && filepath.IsLocal(c) &&
		!strings.ContainsAny(c, `/\:`) && strings.IndexFunc(c, unicode.IsControl) < 0 &&
		!strings.HasSuffix(c, ".") && !strings.HasSuffix(c, " ")
}

const (
	blockSize = 16 << 10 // BitTorrent's request size; buffers track blocks
	// maxBuffered caps memory used by unfinished pieces per torrent. Past
	// it, the oldest partial piece is written to disk early.
	maxBuffered = 256 << 20
)

func (s *fileStore) OpenTorrent(_ context.Context, info *metainfo.Info, ih metainfo.Hash) (storage.TorrentImpl, error) {
	s.mu.Lock()
	s.ih = ih // for its record, from here on (see unavailableNow)
	s.mu.Unlock()
	ft := &fileTorrent{
		store:   s,
		info:    info,
		pc:      s.pc,
		ih:      ih,
		index:   info.FileSegmentsIndex(),
		handles: map[int]*handlePool{},
		bufs:    map[int]*pieceBuf{},
		busy:    map[int]int{},
		lent:    map[*os.File]bool{},
		stale:   map[*os.File]bool{},
	}
	paths, err := torrentPaths(s.dir, info)
	if err == nil {
		if s.ownedDir != "" {
			s.mu.Lock()
			s.mount = recordedMount(recordPath(s.ownedDir, ih, s.dir))
			s.mu.Unlock()
		}
		if gone := s.unavailableNow(); gone != nil {
			// Claimed once it's back (see reclaim): its files, and so its
			// ownership record, can't be checked now.
			s.mu.Lock()
			s.gone = gone
			s.mu.Unlock()
		} else {
			err = s.claim(ih, info, paths)
		}
	}
	if err != nil {
		// Blocked: no file is opened (see acquire), and the engine stops
		// the torrent and shows why.
		s.block(err)
		ft.blocked = err
		paths = make([]string, len(info.UpvertedFiles()))
	}
	ft.paths = paths
	s.mu.Lock()
	ft.own = s.own
	s.mu.Unlock()
	for _, fi := range info.UpvertedFiles() {
		ft.lengths = append(ft.lengths, fi.Length)
	}
	ft.freed = sync.NewCond(&ft.mu)
	s.mu.Lock()
	s.ft = ft
	s.mu.Unlock()
	return storage.TorrentImpl{
		Piece: func(p metainfo.Piece) storage.PieceImpl {
			return &filePiece{t: ft, p: p}
		},
		Close: ft.close,
	}, nil
}

func isSub(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

type fileTorrent struct {
	store   *fileStore
	info    *metainfo.Info
	pc      storage.PieceCompletion
	ih      metainfo.Hash
	index   segments.Index
	paths   []string
	lengths []int64
	blocked error      // see fileStore.claim; no file is opened if set
	own     *ownership // the store's, fixed once opened; nil without records

	mu            sync.Mutex
	freed         *sync.Cond          // signalled when a handle is released
	handles       map[int]*handlePool // open handles, by file index
	bufs          map[int]*pieceBuf   // unfinished pieces, by piece index
	buffered      int64
	seq           uint64 // allocation order, to evict the oldest buffer
	closed        bool
	inUse         int         // handles handed out and not yet released, plus MarkCompletes running
	closeTimedOut bool        // close stopped waiting for inUse
	busy          map[int]int // handles in use, by file index
	// lent are the handles in use; stale, those of them opened before the
	// download folder went away and came back (dropHandles): closed, not
	// reused, when they're released. By handle: a fresh one opened since
	// mustn't be taken for an old one.
	lent  map[*os.File]bool
	stale map[*os.File]bool
	// failLog, while Verify runs, collects the pieces that failed their
	// check (see MarkNotComplete); nil otherwise.
	failLog map[int]bool
	// readFailed: pieces whose data couldn't be read (an error, not a
	// mismatch) since their last check. Their check fails too, but they
	// aren't damaged: MarkNotComplete keeps their record. Cleared when the
	// piece is completed and when a check starts (see startFailLog), so an
	// old failure (serving a peer while the drive was away) can't excuse a
	// piece that's really damaged.
	readFailed map[int]bool
	// unreadLog, while Verify runs, collects the pieces that couldn't be
	// read (not counted as damaged, and not verified either).
	unreadLog map[int]bool
}

// pieceBuf holds a piece's blocks until it's complete. have marks which
// blocks were written into it.
type pieceBuf struct {
	data []byte
	have []bool
	seq  uint64
}

// handlesPerFile is how many handles a file may have open at once. On
// Windows, reads and writes through a single *os.File run one at a time, so
// with one handle every piece write waited for the previous one's network
// round trip to the NAS. Several handles keep several writes in flight.
const handlesPerFile = 8

type handlePool struct {
	idle []*os.File
	open int // handles opened, idle or in use
}

// acquire returns an open handle for file i, waiting if all of the file's
// handles are busy. With create false, a missing file returns
// fs.ErrNotExist instead of being created (reads of data we don't have,
// e.g. skipped files, shouldn't leave empty files behind). A newly created
// file is made sparse and sized to its full length. Call release after use.
func (t *fileTorrent) acquire(i int, create bool) (*os.File, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.blocked != nil {
		return nil, t.blocked
	}
	if err := t.store.Unavailable(); err != nil {
		return nil, err
	}
	for {
		if t.closed {
			return nil, errors.New("torrent storage closed")
		}
		pool := t.handles[i]
		if pool == nil {
			pool = &handlePool{}
			t.handles[i] = pool
		}
		if n := len(pool.idle); n > 0 {
			f := pool.idle[n-1]
			pool.idle = pool.idle[:n-1]
			t.inUse++
			t.busy[i]++
			t.lent[f] = true
			return f, nil
		}
		if pool.open < handlesPerFile {
			f, err := t.openFile(i, create)
			if err != nil {
				if t.store.checkGone() {
					return nil, t.store.Unavailable()
				}
				return nil, err
			}
			pool.open++
			t.inUse++
			t.busy[i]++
			t.lent[f] = true
			return f, nil
		}
		t.freed.Wait()
	}
}

func (t *fileTorrent) release(i int, f *os.File) { t.giveBack(i, f, false) }

// discard gives back a handle that failed a read or write: it's closed, not
// reused. On a network drive a handle can go bad for good (the connection
// dropped and came back) while the folder looks fine; reusing it made every
// write fail ("An unexpected network error occurred"), so the torrent
// downloaded the same pieces over and over without saving them.
func (t *fileTorrent) discard(i int, f *os.File) { t.giveBack(i, f, true) }

func (t *fileTorrent) giveBack(i int, f *os.File, bad bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inUse--
	if t.busy[i]--; t.busy[i] <= 0 {
		delete(t.busy, i)
	}
	delete(t.lent, f)
	if t.stale[f] {
		delete(t.stale, f)
		bad = true
	}
	if t.closed {
		f.Close()
		t.freed.Broadcast() // waitReleased
		return
	}
	if bad {
		t.store.markSuspect("reading or writing its files failed")
		f.Close()
		t.handles[i].open--
		t.freed.Signal() // a handle can be opened in its place
		return
	}
	t.handles[i].idle = append(t.handles[i].idle, f)
	t.freed.Signal()
}

// dropHandles closes the handles opened before the download folder went
// away (they belong to a connection that no longer exists); those in use
// are closed when they're given back. New ones are opened as needed.
func (t *fileTorrent) dropHandles() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, pool := range t.handles {
		for _, f := range pool.idle {
			f.Close()
			pool.open--
		}
		pool.idle = nil
	}
	for f := range t.lent {
		t.stale[f] = true
	}
	t.freed.Broadcast()
}

// waitReleased waits (up to timeout) until the closed torrent's last file
// handle is closed and no piece is still being written, and returns the
// files still open after that. Deleting a file that's still open fails on
// Windows and, on an SMB share, leaves a ".__smbfile_silly" file and can
// stall the share.
func (s *fileStore) waitReleased(timeout time.Duration) (stillOpen map[string]bool) {
	s.mu.Lock()
	t := s.ft
	s.mu.Unlock()
	if t == nil {
		return nil
	}
	timedOut := false
	timer := time.AfterFunc(timeout, func() {
		t.mu.Lock()
		timedOut = true
		t.freed.Broadcast()
		t.mu.Unlock()
	})
	defer timer.Stop()
	t.mu.Lock()
	defer t.mu.Unlock()
	for t.inUse > 0 && !timedOut {
		t.freed.Wait()
	}
	if len(t.busy) == 0 {
		return nil
	}
	stillOpen = map[string]bool{}
	for i := range t.busy {
		stillOpen[t.paths[i]] = true
	}
	return stillOpen
}

// openFile opens (or creates) file i. Called with t.mu held.
func (t *fileTorrent) openFile(i int, create bool) (*os.File, error) {
	f, err := os.OpenFile(t.paths[i], os.O_RDWR, 0o644)
	if errors.Is(err, fs.ErrNotExist) && create {
		return t.createFile(i)
	}
	if err != nil {
		return nil, err
	}
	if err := t.ours(i); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ours refuses file i if the store didn't create it: it appeared at the
// torrent's path after the torrent was checked.
func (t *fileTorrent) ours(i int) error {
	if t.own == nil || t.own.has(t.paths[i]) {
		return nil
	}
	return fmt.Errorf("%s wasn't created by Shark BiteTorrent, so it won't write into it", filepath.Base(t.paths[i]))
}

// createFile creates file i, which mustn't exist yet, sparse and sized to
// its full length, and records it (and any folders made for it) as the
// torrent's own.
func (t *fileTorrent) createFile(i int) (*os.File, error) {
	p := t.paths[i]
	// Not onto the empty folder a drive that went away left behind.
	if t.store.checkGone() {
		return nil, t.store.Unavailable()
	}
	tag := newTag(t.lengths[i])
	if t.own != nil {
		if err := t.own.pending(p, tag); err != nil {
			return nil, err
		}
	}
	if err := t.mkdirs(filepath.Dir(p)); err != nil {
		t.cancel(p)
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.cancel(p) // there already (someone else's), or can't be made
		return nil, err
	}
	makeSparse(f) // best effort: unsupported filesystems still work, just slower
	if _, err = f.WriteAt(tag, 0); err != nil {
		err = fmt.Errorf("can't create %s: %w", filepath.Base(p), err)
	} else if err = f.Truncate(t.lengths[i]); err != nil {
		// Too big for the drive (FAT32), full, over quota: don't leave a
		// stub behind.
		err = fmt.Errorf("can't create %s: %w", filepath.Base(p), err)
	} else if t.own != nil {
		err = t.own.add(p)
	}
	if err != nil {
		f.Close()
		os.Remove(p)
		t.cancel(p)
		return nil, err
	}
	// Marked as downloaded from the internet from the start, so a file
	// opened before the torrent finishes is checked by the OS.
	scan.MarkDownloaded(p)
	return f, nil
}

func (t *fileTorrent) cancel(p string) {
	if t.own != nil {
		t.own.cancel(p)
	}
}

// mkdirs creates folder d inside the download folder, recording each one it
// makes, so removing the torrent prunes only folders it created.
func (t *fileTorrent) mkdirs(d string) error {
	root := filepath.Clean(t.store.dir)
	var missing []string
	for ; d != root && isSub(root, d); d = filepath.Dir(d) {
		fi, err := os.Lstat(d)
		if err == nil {
			if !fi.IsDir() {
				rel, _ := filepath.Rel(root, d)
				return fmt.Errorf("%q in the download folder is a link or a file, not a folder", rel)
			}
			break
		}
		missing = append(missing, d)
	}
	// The download folder itself is never created here (the app makes it
	// before adding a torrent): if it's gone, its drive isn't connected (a
	// network drive's mount point on Linux is an empty folder), and making
	// it would download onto this computer's own disk instead.
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return fmt.Errorf("the download folder %s isn't available; if it's on a network drive or another disk, connect it", root)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue // made by someone else meanwhile: not ours
			}
			return err
		}
		if t.own != nil {
			if err := t.own.addDir(missing[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *fileTorrent) close() error {
	t.mu.Lock()
	t.closed = true
	bufs := t.bufs
	t.bufs, t.buffered = map[int]*pieceBuf{}, 0 // unfinished pieces are re-downloaded later
	t.mu.Unlock()
	t.store.mu.Lock()
	removed := t.store.removed
	t.store.mu.Unlock()
	if !removed {
		// In the background, bounded: one write stuck on a dead network
		// drive must not hang quitting. Pieces it doesn't get to are
		// downloaded again.
		deadline := time.Now().Add(15 * time.Second)
		done := make(chan struct{})
		go func() {
			defer close(done)
			t.writeFinished(bufs, deadline)
		}()
		select {
		case <-done:
		case <-time.After(time.Until(deadline) + time.Second):
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Let pieces already being written finish and record themselves before
	// the progress database is closed after this (a budget of their own).
	timer := time.AfterFunc(10*time.Second, func() {
		t.mu.Lock()
		t.closeTimedOut = true
		t.freed.Broadcast()
		t.mu.Unlock()
	})
	defer timer.Stop()
	for t.inUse > 0 && !t.closeTimedOut {
		t.freed.Wait()
	}
	var errs []error
	for i, pool := range t.handles {
		for _, f := range pool.idle {
			errs = append(errs, f.Close())
		}
		delete(t.handles, i) // handles still in use are closed on release
	}
	t.freed.Broadcast()
	return errors.Join(errs...)
}

// writeFinished writes out, and records as finished, pieces that were
// complete in memory when the torrent closed. Pieces are written when the
// library marks them complete, and on a slow disk (a NAS) a backlog of
// complete, hash-checked pieces waiting for that built up to maxBuffered;
// quitting threw it away, so up to 256 MB downloaded again after a restart.
// Each piece's hash is checked here first; nothing unverified is recorded.
func (t *fileTorrent) writeFinished(bufs map[int]*pieceBuf, deadline time.Time) {
	for idx, buf := range bufs {
		if time.Now().After(deadline) {
			return // the rest downloads again: quitting mustn't hang
		}
		piece := t.info.Piece(idx)
		want := piece.V1Hash()
		if !want.Ok || int64(len(buf.data)) < piece.Length() || !covered(buf, 0, int(piece.Length())) {
			continue
		}
		data := buf.data[:piece.Length()]
		if sha1.Sum(data) != want.Value {
			continue
		}
		ext := segments.Extent{Start: piece.Offset(), Length: piece.Length()}
		ok, n := true, int64(0)
		for i, e := range t.index.LocateIter(ext) {
			if e.Length == 0 {
				continue
			}
			// Blocked torrents have nothing buffered. The file may not exist
			// yet if this is its first piece to reach the disk.
			f, err := os.OpenFile(t.paths[i], os.O_WRONLY, 0o644)
			if err == nil {
				if err = t.ours(i); err != nil {
					f.Close()
				}
			} else if errors.Is(err, fs.ErrNotExist) {
				f, err = t.createFile(i)
			}
			if err == nil {
				_, err = f.WriteAt(data[n:n+e.Length], e.Start)
				if err == nil {
					err = f.Sync() // on the drive before it's recorded (see syncData)
				}
				// A network drive can report a failed write only here; the
				// piece is recorded only if this succeeds too.
				if cerr := f.Close(); err == nil {
					err = cerr
				}
			}
			if err != nil {
				ok = false
				break
			}
			n += e.Length
		}
		if ok {
			t.pc.Set(metainfo.PieceKey{InfoHash: t.ih, Index: idx}, true)
		}
	}
}

type filePiece struct {
	t *fileTorrent
	p metainfo.Piece
}

func (p *filePiece) key() metainfo.PieceKey {
	return metainfo.PieceKey{InfoHash: p.t.ih, Index: p.p.Index()}
}

// diskIO runs fn over each file segment covered by [off, off+len(b)) of the piece.
func (p *filePiece) diskIO(b []byte, off int64, create bool,
	fn func(f *os.File, b []byte, off int64) (int, error)) (n int, err error) {
	ext := segments.Extent{Start: p.p.Offset() + off, Length: int64(len(b))}
	for i, e := range p.t.index.LocateIter(ext) {
		if e.Length == 0 {
			continue
		}
		f, err := p.t.acquire(i, create)
		if errors.Is(err, fs.ErrNotExist) {
			return n, io.EOF // data we don't have reads as a short read
		}
		if err != nil {
			return n, err
		}
		m, err := fn(f, b[n:n+int(e.Length)], e.Start)
		if err != nil && !errors.Is(err, io.EOF) {
			p.t.discard(i, f)
		} else {
			p.t.release(i, f)
		}
		n += m
		if err != nil {
			if p.t.store.checkGone() {
				return n, p.t.store.Unavailable()
			}
			return n, err
		}
	}
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

// ReadAt reads from disk, then overlays any blocks still in the piece's
// buffer (so hashing sees the blocks that haven't been written out yet).
func (p *filePiece) ReadAt(b []byte, off int64) (int, error) {
	p.t.mu.Lock()
	buf := p.t.bufs[p.p.Index()]
	if buf != nil && covered(buf, off, len(b)) {
		n := copy(b, buf.data[off:])
		p.t.mu.Unlock()
		return n, nil
	}
	p.t.mu.Unlock()

	n, err := p.diskIO(b, off, false, (*os.File).ReadAt)
	if err != nil && !errors.Is(err, io.EOF) {
		p.t.mu.Lock()
		if p.t.readFailed == nil {
			p.t.readFailed = map[int]bool{}
		}
		p.t.readFailed[p.p.Index()] = true
		p.t.mu.Unlock()
	}
	if buf != nil {
		p.t.mu.Lock()
		if cur := p.t.bufs[p.p.Index()]; cur == buf {
			overlay(buf, b, off)
		}
		p.t.mu.Unlock()
	}
	return n, err
}

// WriteAt buffers a block in memory. Writes that aren't whole, aligned
// blocks go straight to disk.
func (p *filePiece) WriteAt(b []byte, off int64) (int, error) {
	plen := p.p.Length()
	aligned := off%blockSize == 0 && (len(b) == blockSize || off+int64(len(b)) == plen)
	if !aligned {
		return p.diskIO(b, off, true, (*os.File).WriteAt)
	}
	idx := p.p.Index()

	p.t.mu.Lock()
	buf := p.t.bufs[idx]
	var evict *pieceBuf
	var evictIdx int
	if buf == nil {
		p.t.seq++
		buf = &pieceBuf{
			data: make([]byte, plen),
			have: make([]bool, (plen+blockSize-1)/blockSize),
			seq:  p.t.seq,
		}
		p.t.bufs[idx] = buf
		p.t.buffered += plen
		if p.t.buffered > maxBuffered {
			evictIdx, evict = p.t.oldestOtherThan(idx)
			if evict != nil {
				delete(p.t.bufs, evictIdx)
				p.t.buffered -= int64(len(evict.data))
			}
		}
	}
	copy(buf.data[off:], b)
	buf.have[off/blockSize] = true
	p.t.mu.Unlock()

	if evict != nil {
		// Memory cap reached: write the oldest partial piece out now.
		p.flushAt(evictIdx, evict)
	}
	return len(b), nil
}

func (t *fileTorrent) oldestOtherThan(skip int) (int, *pieceBuf) {
	var oi int
	var ob *pieceBuf
	for i, b := range t.bufs {
		if i != skip && (ob == nil || b.seq < ob.seq) {
			oi, ob = i, b
		}
	}
	return oi, ob
}

// flushAt writes buf's blocks for piece idx to disk, as few large writes as
// possible (contiguous runs of blocks). idx may be a different piece than p
// (when evicting the oldest buffer to stay under the memory cap).
func (p *filePiece) flushAt(idx int, buf *pieceBuf) error {
	piece := p
	if idx != p.p.Index() {
		piece = &filePiece{t: p.t, p: p.t.info.Piece(idx)}
	}
	var errs []error
	for i := 0; i < len(buf.have); {
		if !buf.have[i] {
			i++
			continue
		}
		j := i
		for j < len(buf.have) && buf.have[j] {
			j++
		}
		lo := int64(i) * blockSize
		hi := min(int64(j)*blockSize, int64(len(buf.data)))
		if _, err := piece.diskIO(buf.data[lo:hi], lo, true, (*os.File).WriteAt); err != nil {
			errs = append(errs, err)
		}
		i = j
	}
	return errors.Join(errs...)
}

// syncData waits until the piece's data is on the drive (fsync /
// FlushFileBuffers on each file it covers). Without it a network drive's
// client keeps writes in its cache and reports them done: if the connection
// then drops, the cached data is lost but the piece was already recorded as
// finished, so the download completed with damaged pieces (a CachyOS ISO
// failed its checksum after a mapped drive was disconnected mid-download).
// Costs about 10% of write speed on a NAS.
func (p *filePiece) syncData() error {
	ext := segments.Extent{Start: p.p.Offset(), Length: p.p.Length()}
	for i, e := range p.t.index.LocateIter(ext) {
		if e.Length == 0 {
			continue
		}
		f, err := p.t.acquire(i, false)
		if err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			p.t.discard(i, f)
			if p.t.store.checkGone() {
				return p.t.store.Unavailable()
			}
			return err
		}
		p.t.release(i, f)
	}
	return nil
}

func (p *filePiece) MarkComplete() error {
	idx := p.p.Index()
	p.t.mu.Lock()
	if p.t.closed {
		p.t.mu.Unlock()
		return errors.New("torrent storage closed")
	}
	p.t.inUse++ // waitReleased waits for this
	defer func() {
		p.t.mu.Lock()
		p.t.inUse--
		p.t.freed.Broadcast()
		p.t.mu.Unlock()
	}()
	buf := p.t.bufs[idx]
	if buf != nil {
		delete(p.t.bufs, idx)
		p.t.buffered -= int64(len(buf.data))
	}
	p.t.mu.Unlock()
	if buf != nil {
		if err := p.flushAt(idx, buf); err != nil {
			p.t.store.markFailed(idx)
			return err
		}
	}
	if err := p.syncData(); err != nil {
		p.t.store.markFailed(idx)
		return err // not on the drive for sure: not recorded
	}
	p.t.store.mu.Lock()
	removed := p.t.store.removed
	p.t.store.mu.Unlock()
	if removed {
		// Removed meanwhile: don't leave a "complete" record behind. (On a
		// normal close the piece is recorded: its data is written, and close
		// waits for this, so quitting doesn't lose finished pieces.)
		return errors.New("torrent storage closed")
	}
	p.t.mu.Lock()
	delete(p.t.readFailed, idx)
	p.t.mu.Unlock()
	return p.t.pc.Set(p.key(), true)
}

func (p *filePiece) MarkNotComplete() error {
	idx := p.p.Index()
	p.t.mu.Lock()
	if p.t.readFailed[idx] {
		// Its check failed because the data couldn't be read (the drive
		// refused access, say, right after the app was killed: a NAS keeps
		// the old handle open for a while), not because it's damaged. Its
		// record stays; the library re-downloads it this session.
		delete(p.t.readFailed, idx)
		if p.t.unreadLog != nil {
			p.t.unreadLog[idx] = true
		}
		p.t.mu.Unlock()
		p.t.store.markSuspect("its files couldn't be read")
		return nil
	}
	if p.t.failLog != nil {
		p.t.failLog[idx] = true
	}
	if buf := p.t.bufs[idx]; buf != nil {
		delete(p.t.bufs, idx) // failed the hash check: discard
		p.t.buffered -= int64(len(buf.data))
	}
	p.t.mu.Unlock()
	// A check that failed because the drive is away read nothing: a piece
	// recorded as finished keeps its record (forgetting it would download
	// it again once the drive is back).
	if p.t.store.Unavailable() != nil {
		return nil
	}
	return p.t.pc.Set(p.key(), false)
}

// Completion trusts the database, but double-checks that the files covering
// the piece still exist at full size (they may have been deleted outside the
// app).
func (p *filePiece) Completion() storage.Completion {
	c, err := p.t.pc.Get(p.key())
	if err != nil {
		return storage.Completion{Err: err}
	}
	if !c.Ok || !c.Complete {
		return c
	}
	if p.t.store.Blocked() != nil {
		return storage.Completion{Ok: true, Complete: false}
	}
	if p.t.store.Unavailable() != nil {
		// Its files can't be looked at (the drive is disconnected): the
		// database's word, not "missing", or everything would download
		// again, over the files, once the drive is back.
		return c
	}
	ext := segments.Extent{Start: p.p.Offset(), Length: p.p.Length()}
	for i, e := range p.t.index.LocateIter(ext) {
		fi, err := os.Stat(p.t.paths[i])
		if err != nil && p.t.store.checkGone() {
			return c // the drive went away while running: as above
		}
		if err != nil || fi.Size() < e.Start+e.Length {
			return storage.Completion{Ok: true, Complete: false}
		}
	}
	return c
}

func covered(buf *pieceBuf, off int64, n int) bool {
	for i := off / blockSize; i*blockSize < off+int64(n); i++ {
		if int(i) >= len(buf.have) || !buf.have[i] {
			return false
		}
	}
	return true
}

func overlay(buf *pieceBuf, b []byte, off int64) {
	for i := off / blockSize; i*blockSize < off+int64(len(b)); i++ {
		if int(i) >= len(buf.have) || !buf.have[i] {
			continue
		}
		lo := max(i*blockSize, off)
		hi := min((i+1)*blockSize, off+int64(len(b)), int64(len(buf.data)))
		copy(b[lo-off:hi-off], buf.data[lo:hi])
	}
}

// startFailLog begins collecting the pieces that fail their check; the
// returned func stops and returns them, sorted.
func (s *fileStore) startFailLog() func() (bad, unreadable []int) {
	s.mu.Lock()
	t := s.ft
	s.mu.Unlock()
	if t == nil {
		return func() ([]int, []int) { return nil, nil }
	}
	t.mu.Lock()
	t.failLog, t.unreadLog = map[int]bool{}, map[int]bool{}
	t.readFailed = nil // only this check's read errors count
	t.mu.Unlock()
	return func() ([]int, []int) {
		t.mu.Lock()
		defer t.mu.Unlock()
		keys := func(m map[int]bool) []int {
			out := make([]int, 0, len(m))
			for i := range m {
				out = append(out, i)
			}
			slices.Sort(out)
			return out
		}
		bad, unreadable := keys(t.failLog), keys(t.unreadLog)
		t.failLog, t.unreadLog = nil, nil
		return bad, unreadable
	}
}

// clearReadFailed forgets old read errors before the library re-checks
// pieces (see readFailed).
func (s *fileStore) clearReadFailed() {
	s.mu.Lock()
	t := s.ft
	s.mu.Unlock()
	if t != nil {
		t.mu.Lock()
		t.readFailed = nil
		t.mu.Unlock()
	}
}

// markFailed reports a piece whose data couldn't be saved: the library
// counts it finished anyway (it ignores MarkComplete's error), so the
// engine has it checked again (see onMarkFailed).
func (s *fileStore) markFailed(idx int) {
	s.mu.Lock()
	f := s.onMarkFailed
	s.mu.Unlock()
	if f != nil {
		f(idx)
	}
}
