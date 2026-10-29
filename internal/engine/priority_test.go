package engine

import (
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// A torrent of three files on a client with no network: just enough for the
// library to compute priorities.
func priorityTorrent(t *testing.T) *torrent.Torrent {
	t.Helper()
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.NoDHT, cfg.DisableTCP, cfg.DisableUTP, cfg.NoUpload = true, true, true, true
	cfg.DisableTrackers, cfg.DisableWebtorrent, cfg.DisableWebseeds = true, true, true
	cfg.ListenPort = 0
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })

	const pieceLen = 16 << 10
	info := metainfo.Info{Name: "set", PieceLength: pieceLen, Files: []metainfo.FileInfo{
		{Path: []string{"a.mkv"}, Length: 5 * pieceLen}, // pieces 0-4
		{Path: []string{"b.nfo"}, Length: pieceLen / 2}, // piece 5 (shared with c)
		{Path: []string{"c.mkv"}, Length: 4*pieceLen + pieceLen/2},
	}}
	total := int64(0)
	for _, f := range info.Files {
		total += f.Length
	}
	info.Pieces = make([]byte, 20*((total+pieceLen-1)/pieceLen))
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	spec := &torrent.TorrentSpec{}
	spec.InfoHash, spec.InfoBytes = metainfo.HashBytes(ib), ib
	tt, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	<-tt.GotInfo()
	// A piece's reported priority is its effective one, None until its
	// state on disk is known: wait for the (empty) storage to be checked.
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; i < tt.NumPieces(); {
		if tt.PieceState(i).Ok && !tt.PieceState(i).Checking {
			i++
			continue
		}
		if time.Now().After(deadline) {
			t.Fatalf("piece %d's state never became known", i)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return tt
}

func TestApplyPriorities(t *testing.T) {
	tt := priorityTorrent(t)
	files := tt.Files()
	en := &entry{t: tt, skipped: map[int]bool{1: true}, high: map[int]bool{2: true}}
	e := &Engine{}
	e.applyPriorities(en)

	if p := files[0].Priority(); p != torrent.PiecePriorityNormal {
		t.Errorf("a.mkv: %v, want Normal", p)
	}
	if p := files[1].Priority(); p != torrent.PiecePriorityNone {
		t.Errorf("b.nfo (skipped): %v, want None", p)
	}
	if p := files[2].Priority(); p != torrent.PiecePriorityHigh {
		t.Errorf("c.mkv (high): %v, want High", p)
	}
	piece := func(i int) torrent.PiecePriority { return tt.PieceState(i).Priority }
	// Without first-and-last, a.mkv's middle and ends are all Normal.
	if piece(0) != torrent.PiecePriorityNormal || piece(2) != torrent.PiecePriorityNormal || piece(4) != torrent.PiecePriorityNormal {
		t.Errorf("a.mkv pieces 0/2/4: %v %v %v, want all Normal", piece(0), piece(2), piece(4))
	}

	// First and last pieces first: a.mkv's ends come before everything else
	// (Next), its middle stays.
	en.firstLast = true
	e.applyPriorities(en)
	if piece(0) != torrent.PiecePriorityNext || piece(4) != torrent.PiecePriorityNext {
		t.Errorf("first-and-last: a.mkv ends %v %v, want Next", piece(0), piece(4))
	}
	if piece(2) != torrent.PiecePriorityNormal {
		t.Errorf("first-and-last: a.mkv middle %v, want Normal", piece(2))
	}
	// Piece 5 holds the skipped b.nfo and the start of the wanted c.mkv: the
	// wanted file's boost wins.
	if piece(5) != torrent.PiecePriorityNext {
		t.Errorf("shared piece 5: %v, want Next (c.mkv's first piece)", piece(5))
	}
	// A High file's ends still come before its middle (seen: with the whole
	// video High and its ends High too, nothing set them apart).
	if piece(9) != torrent.PiecePriorityNext || piece(7) != torrent.PiecePriorityHigh {
		t.Errorf("high c.mkv: last piece %v (want Next), middle %v (want High)", piece(9), piece(7))
	}

	// Off again: back to the files' own priorities.
	en.firstLast = false
	e.applyPriorities(en)
	if piece(0) != torrent.PiecePriorityNormal || piece(4) != torrent.PiecePriorityNormal {
		t.Errorf("after turning it off: a.mkv ends %v %v, want Normal", piece(0), piece(4))
	}
}
