package main

import (
	"context"
	"fmt"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"sharkbite/internal/engine"
)

// Seeding limits: a finished torrent stops seeding once it has uploaded
// enough (Settings.SeedRatio) or seeded long enough (Settings.SeedHours).
// The totals behind them are kept in savedTorrent, since the engine counts
// only since it started (it restarts with each VPN connection).

// totalsEvery: how often the totals are written while nothing else is.
const totalsEvery = time.Minute

func (a *App) seedLoop(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		eng, err := a.engine()
		if err != nil {
			continue
		}
		now := time.Now()
		// A late tick means the computer slept, and nothing seeded meanwhile.
		dt := min(now.Sub(last), 30*time.Second)
		last = now
		for _, hash := range a.updateTotals(eng.List(), dt, false) {
			a.stopSeeding(hash)
		}
	}
}

// updateTotals records the torrents' totals and seeding time (dt more for
// those seeding) and returns those that just reached the seeding limit.
// final: the engine is closing; write the totals and check nothing.
func (a *App) updateTotals(list []engine.Status, dt time.Duration, final bool) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var due []string
	for _, t := range list {
		s := a.savedFor(t.Hash)
		if s == nil {
			continue
		}
		// Never less: a list from an engine that just closed can be older.
		s.Uploaded, s.Downloaded = max(s.Uploaded, t.Uploaded), max(s.Downloaded, t.Downloaded)
		if final || t.State != "seeding" {
			continue
		}
		s.SeedMillis += dt.Milliseconds()
		// Once reached, it isn't stopped again: resuming it means keep seeding.
		if !s.SeedLimitHit && seedLimitReached(a.settings, ratioOf(s.Uploaded, s.Downloaded, t.Size), s.SeedMillis) {
			s.SeedLimitHit = true
			due = append(due, t.Hash)
		}
	}
	if final || len(due) > 0 || time.Since(a.totalsSaved) >= totalsEvery {
		a.totalsSaved = time.Now()
		if err := a.writeSaved(); err != nil {
			runtime.LogErrorf(a.ctx, "%s", engine.Redact(fmt.Sprintf("save torrents: %v", err)))
		}
	}
	return due
}

func (a *App) stopSeeding(hash string) {
	a.mu.Lock()
	remove := a.settings.SeedAction == seedRemove
	a.mu.Unlock()
	var err error
	if remove {
		err = a.Remove(hash, false) // keeps the files
	} else {
		err = a.setPausedShown(hash, true, false, -1)
	}
	if err != nil {
		runtime.LogErrorf(a.ctx, "%s", engine.Redact(fmt.Sprintf("seeding limit for %s: %v", hash, err)))
	}
}

const seedRemove = "remove"

func seedLimitReached(s Settings, ratio float64, seedMillis int64) bool {
	return s.SeedRatio > 0 && ratio >= s.SeedRatio ||
		s.SeedHours > 0 && float64(seedMillis) >= s.SeedHours*float64(time.Hour/time.Millisecond)
}

// ratioOf: uploaded ÷ downloaded. Divided by the size when less than that
// was downloaded (it was already on disk, or added before totals were
// kept), so the ratio isn't inflated.
func ratioOf(uploaded, downloaded, size int64) float64 {
	d := max(downloaded, size)
	if d <= 0 {
		return 0
	}
	return float64(uploaded) / float64(d)
}
