package engine

import (
	"os"
	"path/filepath"
	"testing"

	g "github.com/anacrolix/generics"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// Identities match only if both parts agree wherever both are known.
func TestIdentitySame(t *testing.T) {
	a := fileIdentity{index: 7, created: 1000}
	cases := []struct {
		b    fileIdentity
		want bool
	}{
		{fileIdentity{7, 1000}, true},
		{fileIdentity{8, 1000}, false}, // same (coarse) creation time, other file
		{fileIdentity{7, 2000}, false}, // inode reused by a new file
		{fileIdentity{0, 1000}, !bothNeeded},
		{fileIdentity{7, 0}, true},
		{fileIdentity{0, 0}, false},
	}
	for i, c := range cases {
		if got := a.same(c.b); got != c.want {
			t.Errorf("case %d (%v vs %v): got %v", i, a, c.b, got)
		}
	}
	if (fileIdentity{0, -5}).known() {
		t.Error("a placeholder creation time counted as known")
	}
	if bothNeeded && (fileIdentity{0, 1000}).known() {
		t.Error("Windows: a file without an index counted as identifiable")
	}
}

// An old torrent adopts only files that look like its own: the full length,
// or shorter only when it has finished pieces recorded there.
func TestAdoptLengths(t *testing.T) {
	dir, owned := t.TempDir(), t.TempDir()
	write := func(name string, n int) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	full := write("full.bin", 100)     // the torrent's, complete size
	short := write("short.bin", 40)    // the torrent's, on a NAS reporting less
	users := write("users.bin", 71)    // the user's file at a path never downloaded
	longer := write("longer.bin", 150) // longer than the torrent's file: not it
	o := loadOwnership(owned, hashN(3), dir, true)
	err := o.adopt([]string{full, short, users, longer}, []int64{
		-100 - 1, // no progress: exact length only
		100,      // progress recorded: up to the length
		-100 - 1, // no progress, wrong length
		100,      // progress recorded, but too long
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{full: true, short: true, users: false, longer: false} {
		if got := o.has(p); got != want {
			t.Errorf("%s adopted = %v, want %v", filepath.Base(p), got, want)
		}
	}
}

// anyComplete looks at every piece overlapping the range.
func TestAnyComplete(t *testing.T) {
	pc := storage.NewMapPieceCompletion()
	info := &metainfo.Info{PieceLength: 10, Pieces: make([]byte, 20*5), Length: 50}
	ih := hashN(4)
	pc.Set(metainfo.PieceKey{InfoHash: ih, Index: 2}, g.Some(true)) // bytes 20-29
	if !anyComplete(pc, ih, info, 25, 10) {
		t.Error("piece 2 overlaps [25,35)")
	}
	if anyComplete(pc, ih, info, 30, 20) {
		t.Error("no finished piece in [30,50)")
	}
}
