package main

import (
	"slices"
	"testing"
	"time"

	"sharkbite/internal/engine"
)

func TestRatioOf(t *testing.T) {
	for _, c := range []struct {
		up, down, size int64
		want           float64
	}{
		{300, 100, 100, 3},
		{100, 0, 200, 0.5}, // already on disk: divided by the size
		{100, 400, 200, 0.25},
		{100, 0, 0, 0},
	} {
		if got := ratioOf(c.up, c.down, c.size); got != c.want {
			t.Errorf("ratioOf(%d, %d, %d) = %v, want %v", c.up, c.down, c.size, got, c.want)
		}
	}
}

func TestUpdateTotals(t *testing.T) {
	a := &App{configDir: t.TempDir(), settings: Settings{SeedRatio: 2}}
	a.saved = []savedTorrent{
		{Hash: "a", Uploaded: 500, Downloaded: 1000}, // from an earlier session
		{Hash: "b"},
	}
	list := []engine.Status{
		{Hash: "a", State: "seeding", Size: 1000, Uploaded: 1500, Downloaded: 1000},
		{Hash: "b", State: "downloading", Size: 1000, Uploaded: 5000, Downloaded: 100},
	}
	if due := a.updateTotals(list, 10*time.Second, false); len(due) != 0 {
		t.Fatalf("ratio 1.5: stopped %v", due)
	}
	if a.saved[0].Uploaded != 1500 || a.saved[0].SeedMillis != 10000 || a.saved[1].SeedMillis != 0 {
		t.Fatalf("totals %+v", a.saved)
	}

	// An older list (from an engine that just closed) doesn't lower them.
	a.updateTotals([]engine.Status{{Hash: "a", Uploaded: 100}}, 0, true)
	if a.saved[0].Uploaded != 1500 {
		t.Errorf("uploaded lowered to %d", a.saved[0].Uploaded)
	}

	// Ratio 2 reached: stopped once. Only seeding torrents count ("b" is
	// at 50 but still downloading).
	list[0].Uploaded = 2000
	if due := a.updateTotals(list, 10*time.Second, false); !slices.Equal(due, []string{"a"}) {
		t.Fatalf("ratio 2: stopped %v", due)
	}
	list[0].Uploaded = 3000 // resumed by the user, still seeding
	if due := a.updateTotals(list, 10*time.Second, false); len(due) != 0 {
		t.Errorf("stopped again after being resumed: %v", due)
	}

	// The time limit, which counts only time spent seeding.
	a.settings = Settings{SeedHours: 0.01} // 36 s
	a.saved[0].SeedLimitHit, a.saved[0].SeedMillis = false, 30000
	if due := a.updateTotals(list, 5*time.Second, false); len(due) != 0 {
		t.Fatalf("35 s: stopped %v", due)
	}
	if due := a.updateTotals(list, 5*time.Second, false); len(due) != 1 {
		t.Errorf("40 s: stopped %v", due)
	}

	// The totals reach the disk.
	b := &App{configDir: a.configDir}
	b.loadSaved()
	if len(b.saved) != 2 || b.saved[0].Uploaded != 3000 || !b.saved[0].SeedLimitHit {
		t.Errorf("reloaded %+v", b.saved)
	}
}
