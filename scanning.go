package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"sharkbite/internal/engine"
	"sharkbite/internal/scan"
	"sharkbite/internal/sfv"
)

// Malware checks on finished torrents. The file-list check (scan.Inspect)
// runs in the engine as soon as metadata arrives; this file handles what
// happens once a torrent has finished downloading.

type scanRecord struct {
	Engine  string   `json:"engine"`
	Clean   bool     `json:"clean"`
	Threats []string `json:"threats,omitempty"`
	Error   string   `json:"error,omitempty"`
	// TooLarge: files ClamAV skipped because of their size; CanScanFully:
	// "Scan fully" would check at least one of them.
	TooLarge     []string `json:"tooLarge,omitempty"`
	CanScanFully bool     `json:"canScanFully,omitempty"`
	// Seen: the user has opened the torrent's files panel since, so its row
	// stops flagging the files that were too large.
	Seen bool  `json:"seen,omitempty"`
	At   int64 `json:"at"` // unix seconds
	// Done: the torrent's downloaded bytes when it was checked. Files ticked
	// later make it finish again with more, and it's checked again.
	Done int64 `json:"done,omitempty"`
	// SFV is the .sfv check, if the torrent has one.
	SFV *engine.SFVStatus `json:"sfv,omitempty"`
}

// scanLoop looks for newly finished torrents and checks them, one at a time,
// until ctx ends (the VPN disconnects or the app quits).
func (a *App) scanLoop(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
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
		a.markChecked(eng)
		for _, t := range eng.List() {
			if t.Suspect != "" {
				a.noteSuspect(t.Hash, t.Suspect)
			}
			if t.State != "seeding" || ctx.Err() != nil {
				continue
			}
			a.mu.Lock()
			s := a.savedFor(t.Hash)
			busy := a.scanning[t.Hash]
			a.mu.Unlock()
			if s == nil || busy || s.Scan != nil && t.Done <= s.Scan.Done {
				continue
			}
			wctx, done := a.workCtx(ctx, t.Hash)
			a.checkFinished(wctx, eng, t)
			done()
		}
	}
}

// savedFor returns the saved entry for hash. Call with a.mu held.
func (a *App) savedFor(hash string) *savedTorrent {
	for i := range a.saved {
		if a.saved[i].Hash == hash {
			return &a.saved[i]
		}
	}
	return nil
}

// checkFinished marks a finished torrent's files as downloaded from the
// internet and, if enabled and available, scans them with the antivirus.
func (a *App) checkFinished(ctx context.Context, eng *engine.Engine, t engine.Status) {
	p, _, ok := eng.ContentPath(t.Hash)
	if !ok {
		return
	}
	if !a.autoVerify(ctx, eng, t) {
		return // damaged pieces are being downloaded again; scanned once complete
	}
	// Only the torrent's own files: its folder may hold other files too.
	for _, f := range eng.OwnedFiles(t.Hash) {
		if ctx.Err() != nil {
			return // being removed, or quitting: nothing more on its files
		}
		scan.MarkDownloaded(f)
	}

	a.mu.Lock()
	enabled := a.settings.ScanDownloads
	a.mu.Unlock()
	name, available := scan.Scanner()
	rec := &scanRecord{Engine: name, At: time.Now().Unix(), Done: t.Done}
	switch {
	case !enabled:
		rec.Error = "scanning is turned off in Settings"
	case !available:
		rec.Error = name + " isn't installed"
	default:
		if !a.runScan(ctx, t.Hash, p, false, rec) {
			return // app quitting or VPN reconnecting: try again next time
		}
	}
	// After the antivirus scan, so a torrent's .sfv can't hold it up.
	sfvRes := a.checkSFV(ctx, eng, t.Hash)
	if ctx.Err() != nil {
		return
	}
	rec.SFV = sfvRes

	a.mu.Lock()
	if s := a.savedFor(t.Hash); s != nil {
		s.Scan = rec
		a.writeSaved()
	}
	a.mu.Unlock()

	if sfvRes != nil && len(sfvRes.Bad) > 0 {
		runtime.EventsEmit(a.ctx, "error", fmt.Sprintf(
			"⚠ “%s”: %d file(s) don't match the torrent's .sfv checksums: %v", t.Name, len(sfvRes.Bad), sfvRes.Bad))
	}
	a.warnThreats(t.Name, rec)
}

// runScan scans p with the antivirus into rec, showing the torrent as
// scanning meanwhile. It returns false if ctx ended first.
func (a *App) runScan(ctx context.Context, hash, p string, full bool, rec *scanRecord) bool {
	a.mu.Lock()
	if a.scanning == nil {
		a.scanning = map[string]bool{}
	}
	a.scanning[hash] = true
	a.mu.Unlock()

	sctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	res, err := scan.ScanPath(sctx, p, full)
	cancel()

	a.mu.Lock()
	delete(a.scanning, hash)
	a.mu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		rec.Error = err.Error()
	} else {
		rec.Clean, rec.Threats = res.Clean, res.Threats
		rec.TooLarge, rec.CanScanFully = res.TooLarge, res.CanScanFully
	}
	return true
}

func (a *App) warnThreats(name string, rec *scanRecord) {
	if len(rec.Threats) > 0 {
		runtime.EventsEmit(a.ctx, "error", fmt.Sprintf(
			"⚠ %s found a threat in “%s”: %v. Don't open its files; consider removing it with its files.",
			rec.Engine, name, rec.Threats))
	}
}

// MarkScanSeen records that the user has seen a torrent's scan result in
// its files panel.
func (a *App) MarkScanSeen(hash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.savedFor(hash); s != nil && s.Scan != nil && !s.Scan.Seen {
		s.Scan.Seen = true
		a.writeSaved()
	}
}

// ScanFully scans a finished torrent again with the antivirus's size limits
// raised, for files the normal scan skipped. It returns at once; progress
// and the result show in the torrent list.
func (a *App) ScanFully(hash string) error {
	eng, err := a.engine()
	if err != nil {
		return err
	}
	p, _, ok := eng.ContentPath(hash)
	if !ok {
		return errors.New("torrent not found")
	}
	a.mu.Lock()
	ctx := a.verifyCtx
	s := a.savedFor(hash)
	if s == nil || s.Scan == nil || !s.Scan.CanScanFully || a.scanning[hash] {
		a.mu.Unlock()
		return errors.New("nothing to scan")
	}
	rec := &scanRecord{Engine: s.Scan.Engine, At: time.Now().Unix(), SFV: s.Scan.SFV, Seen: true, Done: s.Scan.Done}
	if a.scanning == nil {
		a.scanning = map[string]bool{}
	}
	a.scanning[hash] = true // now, so a second click is refused
	a.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, done := a.workCtx(ctx, hash)
	go func() {
		defer done()
		if !a.runScan(ctx, hash, p, true, rec) {
			return
		}
		a.mu.Lock()
		if s := a.savedFor(hash); s != nil {
			s.Scan = rec
			a.writeSaved()
		}
		a.mu.Unlock()
		name := hash
		for _, t := range eng.List() {
			if t.Hash == hash {
				name = t.Name
			}
		}
		a.warnThreats(name, rec)
	}()
	return nil
}

// checkSFV checks the torrent's .sfv file(s), if it has any; nil if not.
// While it runs, the torrent shows as checking.
func (a *App) checkSFV(ctx context.Context, eng *engine.Engine, hash string) *engine.SFVStatus {
	files, err := eng.FilePaths(hash)
	if err != nil || len(sfv.Find(files)) == 0 {
		return nil
	}
	a.mu.Lock()
	if a.sfvChecking == nil {
		a.sfvChecking = map[string]bool{}
	}
	a.sfvChecking[hash] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.sfvChecking, hash)
		a.mu.Unlock()
	}()

	res, err := sfv.Check(ctx, files)
	st := &engine.SFVStatus{Checked: res.Checked, Bad: res.Bad, Missing: res.Missing}
	if err != nil {
		st.Error = err.Error()
	}
	return st
}

// VerifyTorrent re-checks a torrent's files on disk against its piece
// hashes and, if it has one, its .sfv. It returns at once; progress and the
// result show in the torrent list.
func (a *App) VerifyTorrent(hash string) error {
	eng, err := a.engine()
	if err != nil {
		return err
	}
	a.mu.Lock()
	ctx := a.verifyCtx
	a.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, done := a.workCtx(ctx, hash)
	go func() {
		defer done()
		res, err := eng.Verify(ctx, hash)
		if err != nil {
			if ctx.Err() == nil {
				runtime.EventsEmit(a.ctx, "error", "Verify: "+err.Error())
			}
			return
		}
		name := hash
		for _, t := range eng.List() {
			if t.Hash == hash {
				name = t.Name
			}
		}
		sfvRes := a.checkSFV(ctx, eng, hash)
		if ctx.Err() != nil {
			return
		}
		if sfvRes != nil {
			a.mu.Lock()
			if s := a.savedFor(hash); s != nil && s.Scan != nil {
				s.Scan.SFV = sfvRes
				a.writeSaved()
			} else {
				// Not finished/scanned yet: keep it out of the saved scan
				// record, or the malware scan would think it already ran.
				if a.sfvResults == nil {
					a.sfvResults = map[string]*engine.SFVStatus{}
				}
				a.sfvResults[hash] = sfvRes
			}
			a.mu.Unlock()
		}
		if res.Bad > 0 {
			// The files changed since the malware scan (it may have scanned
			// the damaged data): scan them again once they're complete.
			a.mu.Lock()
			if s := a.savedFor(hash); s != nil && s.Scan != nil {
				s.Scan = nil
				a.writeSaved()
			}
			if a.autoVerified == nil {
				a.autoVerified = map[string]bool{}
			}
			a.autoVerified[hash] = true // just verified: no second pass
			a.mu.Unlock()
			// A notice, not an error: nothing for the user to do, and the
			// pieces are usually downloaded again within moments.
			runtime.EventsEmit(a.ctx, "notice", fmt.Sprintf(
				"“%s”: %d of %d pieces on disk were damaged (in %v) and are being downloaded again.", name, res.Bad, res.Pieces, res.BadFiles))
		}
	}()
	return nil
}

// withScans adds scan results to the torrent list for the UI.
func (a *App) withScans(list []engine.Status) []engine.Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range list {
		t := &list[i]
		if s := a.savedFor(t.Hash); s != nil {
			t.Confirm = s.Confirm
		}
		if r, ok := a.sfvResults[t.Hash]; ok {
			t.SFV = r
		}
		if s := a.savedFor(t.Hash); s != nil && s.Scan != nil && s.Scan.SFV != nil {
			t.SFV = s.Scan.SFV
		}
		if a.sfvChecking[t.Hash] {
			t.SFV = &engine.SFVStatus{Checking: true}
		}
		if a.scanning[t.Hash] {
			t.ScanState = "scanning"
			continue
		}
		s := a.savedFor(t.Hash)
		if s == nil || s.Scan == nil {
			continue
		}
		t.ScanEngine = s.Scan.Engine
		switch {
		case len(s.Scan.Threats) > 0:
			t.ScanState, t.Threats = "threat", s.Scan.Threats
		case s.Scan.Error != "":
			t.ScanState, t.ScanError = "error", s.Scan.Error
		case len(s.Scan.TooLarge) > 0:
			t.ScanState, t.TooLarge, t.CanScanFully = "toolarge", s.Scan.TooLarge, s.Scan.CanScanFully
			t.ScanSeen = s.Scan.Seen
		case s.Scan.Clean:
			t.ScanState = "clean"
		}
	}
	return list
}

// torrentWork is the background work reading one torrent's files (antivirus
// scan, .sfv check, Verify), so Remove can stop it before deleting them.
type torrentWork struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	n      int // running; guarded by App.mu
}

// workCtx returns a context for work on hash's files, ended by parent or by
// stopWork; call done when the work is finished.
func (a *App) workCtx(parent context.Context, hash string) (context.Context, func()) {
	a.mu.Lock()
	if a.removing[hash] {
		// Being removed: no new work on its files.
		a.mu.Unlock()
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, func() {}
	}
	if a.work == nil {
		a.work = map[string]*torrentWork{}
	}
	w := a.work[hash]
	if w == nil {
		w = &torrentWork{}
		w.ctx, w.cancel = context.WithCancel(context.Background())
		a.work[hash] = w
	}
	w.wg.Add(1)
	w.n++
	a.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(w.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
		a.mu.Lock()
		if w.n--; w.n == 0 && a.work[hash] == w {
			delete(a.work, hash) // nothing left running on it
		}
		a.mu.Unlock()
		w.wg.Done()
	}
}

// stopWork cancels the work on hash's files and waits (up to timeout) for
// it to stop, so nothing has them open when they're deleted.
func (a *App) stopWork(hash string, timeout time.Duration) {
	a.mu.Lock()
	w := a.work[hash]
	delete(a.work, hash)
	a.mu.Unlock()
	if w == nil {
		return
	}
	w.cancel()
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// markChecked flags torrents saved by an older version once they've been
// claimed (their files adopted), so later starts check them like any other.
func (a *App) markChecked(eng *engine.Engine) {
	a.mu.Lock()
	var hashes []string
	for _, s := range a.saved {
		if s.legacy {
			hashes = append(hashes, s.Hash)
		}
	}
	a.mu.Unlock()
	for _, h := range hashes {
		if eng.Claimed(h) {
			a.updateSaved(h, func(s *savedTorrent) { s.Checked, s.legacy = true, false })
		}
	}
}

// noteSuspect saves why a torrent's files should be verified before the
// malware scan (the first reason wins), so it survives a restart.
func (a *App) noteSuspect(hash, why string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.savedFor(hash); s != nil && s.Suspect == "" && s.Scan == nil {
		s.Suspect = why
		a.writeSaved()
	}
}

// autoVerifyMode is Settings.AutoVerify, "suspect" if unset.
func (a *App) autoVerifyMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch a.settings.AutoVerify {
	case "off", "all":
		return a.settings.AutoVerify
	}
	return "suspect"
}

// autoVerify re-checks a finished torrent's files against its piece hashes
// before the malware scan, when Settings ask for it: always, or only if
// something during the download could have damaged them. It reports
// whether to go on and scan: not if damaged pieces were found (they're
// downloaded again, and the complete, verified files are scanned then).
func (a *App) autoVerify(ctx context.Context, eng *engine.Engine, t engine.Status) bool {
	mode := a.autoVerifyMode()
	a.mu.Lock()
	reason := ""
	if s := a.savedFor(t.Hash); s != nil {
		reason = s.Suspect
	}
	if reason == "" && t.Suspect != "" {
		reason = t.Suspect
	}
	done := a.autoVerified[t.Hash]
	a.mu.Unlock()
	switch {
	case mode == "off" || done:
		return true
	case reason != "":
		reason = "because " + reason
	case mode == "all":
		reason = "as set in Settings"
	default:
		return true
	}
	res, err := eng.VerifyAuto(ctx, t.Hash, reason)
	if err != nil && ctx.Err() == nil {
		// Tried again next time (about every 10 s). One that keeps failing
		// (a file another program holds, a permission problem) mustn't hold
		// back the scan forever without a word: after about a minute it's
		// scanned anyway, and the user is told.
		a.mu.Lock()
		if a.verifyFails == nil {
			a.verifyFails = map[string]int{}
		}
		a.verifyFails[t.Hash]++
		giveUp := a.verifyFails[t.Hash] >= 6
		if giveUp {
			if a.autoVerified == nil {
				a.autoVerified = map[string]bool{}
			}
			a.autoVerified[t.Hash] = true
		}
		a.mu.Unlock()
		if !giveUp {
			return false
		}
		runtime.EventsEmit(a.ctx, "notice", fmt.Sprintf(
			"“%s”: couldn't be verified %s (%v), so it was scanned for malware without that check. You can verify it yourself later.",
			t.Name, reason, err))
		return true
	}
	if err != nil {
		return false // quitting: tried again next time
	}
	a.mu.Lock()
	if a.autoVerified == nil {
		a.autoVerified = map[string]bool{}
	}
	a.autoVerified[t.Hash] = true
	if s := a.savedFor(t.Hash); s != nil {
		s.Suspect = ""
		a.writeSaved()
	}
	a.mu.Unlock()
	if res.Bad > 0 {
		runtime.EventsEmit(a.ctx, "notice", fmt.Sprintf(
			"“%s”: verified %s, %d of %d pieces were damaged (in %v). They're being downloaded again, and the files are scanned for malware once complete.",
			t.Name, reason, res.Bad, res.Pieces, res.BadFiles))
		return false
	}
	return true
}
