package engine

import (
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// Handles opened before the download folder went away and came back, and
// handles that failed a write, are closed instead of reused: on a network
// drive they belong to a dropped connection, and reusing them made every
// write fail while the torrent re-downloaded the same pieces.
func TestStaleHandlesNotReused(t *testing.T) {
	dir := t.TempDir()
	info := &metainfo.Info{Name: "f.bin", PieceLength: 16 << 10, Length: 32 << 10, Pieces: make([]byte, 40)}
	st := newFileStore(dir, storage.NewMapPieceCompletion(), "", false)
	impl, err := st.OpenTorrent(t.Context(), info, hashN(5))
	if err != nil {
		t.Fatal(err)
	}
	defer impl.Close()
	ft := st.ft

	idle, err := ft.acquire(0, true)
	if err != nil {
		t.Fatal(err)
	}
	busy, err := ft.acquire(0, true)
	if err != nil {
		t.Fatal(err)
	}
	ft.release(0, idle) // idle in the pool; busy still in use

	st.reclaim() // the folder came back
	if n := len(ft.handles[0].idle); n != 0 {
		t.Errorf("%d old handles still idle after the drive came back", n)
	}
	if _, err := idle.Stat(); err == nil {
		t.Error("an old idle handle wasn't closed")
	}
	ft.release(0, busy) // in use during reclaim: closed when given back
	if _, err := busy.Stat(); err == nil {
		t.Error("an old handle in use wasn't closed when given back")
	}
	if n := ft.handles[0].open; n != 0 {
		t.Errorf("open count %d, want 0", n)
	}

	// A handle that failed is closed too; the next one is fresh.
	f, err := ft.acquire(0, true)
	if err != nil {
		t.Fatal(err)
	}
	ft.discard(0, f)
	if _, err := f.Stat(); err == nil {
		t.Error("a failed handle wasn't closed")
	}
	g, err := ft.acquire(0, true)
	if err != nil {
		t.Fatal(err)
	}
	if g == f {
		t.Error("a failed handle was reused")
	}
	ft.release(0, g)
	if ft.handles[0].open != 1 || len(ft.handles[0].idle) != 1 {
		t.Errorf("pool: open %d idle %d, want 1 and 1", ft.handles[0].open, len(ft.handles[0].idle))
	}
}

// Verify collects failed pieces as they fail, not by looking at what's
// incomplete afterwards (by then a damaged piece may be downloaded again).
func TestFailLog(t *testing.T) {
	info := &metainfo.Info{Name: "f.bin", PieceLength: 16 << 10, Length: 48 << 10, Pieces: make([]byte, 60)}
	st := newFileStore(t.TempDir(), storage.NewMapPieceCompletion(), "", false)
	impl, err := st.OpenTorrent(t.Context(), info, hashN(6))
	if err != nil {
		t.Fatal(err)
	}
	defer impl.Close()
	impl.Piece(info.Piece(2)).MarkNotComplete() // before Verify: not counted
	stop := st.startFailLog()
	impl.Piece(info.Piece(1)).MarkNotComplete()
	impl.Piece(info.Piece(0)).MarkNotComplete()
	if got := stop(); len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("failed pieces %v, want [0 1]", got)
	}
	if got := stop(); len(got) != 0 {
		t.Errorf("second stop returned %v", got)
	}
}

// A torrent becomes suspect on the first problem (the reason is kept) until
// it's verified.
func TestSuspect(t *testing.T) {
	st := newFileStore(t.TempDir(), storage.NewMapPieceCompletion(), "", false)
	if st.Suspect() != "" {
		t.Fatal("suspect from the start")
	}
	st.markSuspect("its drive was disconnected")
	st.markSuspect("reading or writing its files failed")
	if got := st.Suspect(); got != "its drive was disconnected" {
		t.Errorf("reason %q, want the first one", got)
	}
	st.ClearSuspect()
	if st.Suspect() != "" {
		t.Error("still suspect after ClearSuspect")
	}
}

// A piece whose check failed because its data couldn't be read keeps its
// "finished" record and isn't counted as damaged; a real mismatch is both.
func TestUnreadablePieceKeepsRecord(t *testing.T) {
	info := &metainfo.Info{Name: "f.bin", PieceLength: 16 << 10, Length: 32 << 10, Pieces: make([]byte, 40)}
	pc := storage.NewMapPieceCompletion()
	st := newFileStore(t.TempDir(), pc, "", false)
	impl, err := st.OpenTorrent(t.Context(), info, hashN(7))
	if err != nil {
		t.Fatal(err)
	}
	defer impl.Close()
	key := func(i int) metainfo.PieceKey { return metainfo.PieceKey{InfoHash: hashN(7), Index: i} }
	pc.Set(key(0), true)
	pc.Set(key(1), true)

	stop := st.startFailLog()
	st.ft.mu.Lock()
	st.ft.readFailed = map[int]bool{0: true} // piece 0: the read failed
	st.ft.mu.Unlock()
	impl.Piece(info.Piece(0)).MarkNotComplete()
	impl.Piece(info.Piece(1)).MarkNotComplete() // piece 1: really damaged
	bad := stop()

	if c, _ := pc.Get(key(0)); !c.Complete {
		t.Error("an unreadable piece lost its record")
	}
	if c, _ := pc.Get(key(1)); c.Complete {
		t.Error("a damaged piece kept its record")
	}
	if len(bad) != 1 || bad[0] != 1 {
		t.Errorf("counted as damaged: %v, want [1]", bad)
	}
	if st.Suspect() == "" {
		t.Error("an unreadable piece didn't make the torrent suspect")
	}
}
