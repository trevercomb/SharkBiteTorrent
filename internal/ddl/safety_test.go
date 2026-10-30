package ddl

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"sharkbite/internal/engine"
)

// A download whose folder is away (an unmounted drive) waits, and goes on
// once the folder is back.
func TestFolderWait(t *testing.T) {
	old := folderRetry
	folderRetry = 50 * time.Millisecond
	defer func() { folderRetry = old }()
	data := testData(t, 1000)
	srv := rangeServer(t, data, nil)
	dir := t.TempDir()
	var back atomic.Bool
	m := newManager(nil)
	m.FolderReady = func(string) error {
		if !back.Load() {
			return errors.New("the drive isn't mounted")
		}
		return nil
	}
	m.Add(Spec{ID: "a", URL: srv.URL + "/x", Dir: dir})
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := m.Statuses()[0]
		if st.State == Running && st.Waiting != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never waited for the folder: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote into the folder while it was away: %v", entries)
	}
	back.Store(true)
	waitState(t, m, "a", Done)
	checkFile(t, filepath.Join(dir, "test file.bin"), data)
}

// The finished file never replaces one that's already there.
func TestTakeNameKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "a.bin"+PartSuffix)
	os.WriteFile(part, []byte("new"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.bin"), []byte("old"), 0o644)
	got, err := takeName(part, dir, "a.bin")
	if err != nil || got != "a (2).bin" {
		t.Fatalf("takeName = %q, %v", got, err)
	}
	checkFile(t, filepath.Join(dir, "a.bin"), []byte("old"))
	checkFile(t, filepath.Join(dir, "a (2).bin"), []byte("new"))
	if _, err := os.Stat(part); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("part file left behind: %v", err)
	}
}

// Removing a download with its file deletes only the file it created.
func TestRemoveOnlyItsFile(t *testing.T) {
	data := testData(t, 1000)
	srv := rangeServer(t, data, nil)
	dir := t.TempDir()
	m := newManager(nil)
	m.Identify, m.SameFile = engine.FileID, engine.SameFileID
	m.Add(Spec{ID: "a", URL: srv.URL + "/x", Dir: dir})
	m.Add(Spec{ID: "b", URL: srv.URL + "/x", Dir: dir, Name: "b.bin"})
	waitState(t, m, "a", Done)
	waitState(t, m, "b", Done)
	if m.Specs()[0].FileID == "" {
		t.Skip("this filesystem doesn't tell files apart")
	}

	// Another file put in its place since: kept.
	p := filepath.Join(dir, "test file.bin")
	os.Remove(p)
	os.WriteFile(p, []byte("someone else's"), 0o644)
	if err := m.Remove("a", true); err == nil {
		t.Error("deleted a file the download didn't create")
	}
	checkFile(t, p, []byte("someone else's"))

	// Its own file: deleted.
	if err := m.Remove("b", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its own file is still there: %v", err)
	}
}

// A link put where the part file goes isn't written through.
func TestOpenPartRefusesLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("keep"), 0o644)
	part := filepath.Join(dir, "x"+PartSuffix)
	if err := os.Symlink(target, part); err != nil {
		t.Skipf("can't make a symlink here: %v", err)
	}
	if f, _, err := openPart(part); err == nil {
		f.Close()
		t.Fatal("opened a symlink as the part file")
	}
	checkFile(t, target, []byte("keep"))
}

// A saved name that's nothing once made safe leaves it to the server's.
func TestEmptySafeName(t *testing.T) {
	data := testData(t, 1000)
	srv := rangeServer(t, data, nil)
	dir := t.TempDir()
	m := newManager(nil)
	m.Add(Spec{ID: "a", URL: srv.URL + "/x", Dir: dir, Name: ".."})
	waitState(t, m, "a", Done)
	checkFile(t, filepath.Join(dir, "test file.bin"), data)
}
