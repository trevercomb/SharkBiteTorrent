package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"sharkbite/internal/ddl"
	"sharkbite/internal/scan"
)

// Malware checks on finished direct downloads, like those on finished
// torrents (scanning.go): the file is marked as downloaded from the internet
// and, if enabled, scanned with the system antivirus. The result is kept in
// the download's Meta. As with torrents, the app only reports: it never
// deletes or moves a file because of a scan (the antivirus may, on its own).

// DownloadStatus is a direct download as the UI shows it.
type DownloadStatus struct {
	ddl.Status
	// Scan: "" (not checked yet), "scanning", "clean", "threat", "gone" (the
	// file disappeared, likely quarantined by the antivirus), "error" or
	// "off" (scanning is turned off or there's no antivirus).
	Scan       string   `json:"scan"`
	ScanEngine string   `json:"scanEngine"`
	Threats    []string `json:"threats"`
	ScanError  string   `json:"scanError"`
}

type downloadMeta struct {
	Scan *scanRecord `json:"scan,omitempty"`
	Gone bool        `json:"gone,omitempty"`
}

func metaOf(s ddl.Spec) downloadMeta {
	var m downloadMeta
	if len(s.Meta) > 0 {
		json.Unmarshal(s.Meta, &m)
	}
	return m
}

// downloadScanLoop checks newly finished downloads, one at a time, until
// the app quits.
func (a *App) downloadScanLoop(ctx context.Context, m *ddl.Manager) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for _, s := range m.Specs() {
			if ctx.Err() != nil {
				return
			}
			if !s.Done || metaOf(s).Scan != nil {
				continue
			}
			a.checkDownload(ctx, m, s)
		}
	}
}

func (a *App) checkDownload(ctx context.Context, m *ddl.Manager, s ddl.Spec) {
	p := filepath.Join(s.Dir, s.Name)
	meta := metaOf(s)
	name, available := scan.Scanner()
	rec := &scanRecord{Engine: name, At: time.Now().Unix(), Done: s.Size}
	if _, err := os.Stat(p); err != nil {
		// Gone already: removed, or quarantined by the antivirus the moment
		// it was written.
		meta.Gone = true
		rec.Error = "the file is gone (your antivirus may have quarantined it)"
	} else {
		scan.MarkDownloaded(p)
		a.mu.Lock()
		enabled := a.settings.ScanDownloads
		a.mu.Unlock()
		switch {
		case !enabled:
			rec.Error = "scanning is turned off in Settings"
		case !available:
			rec.Error = name + " isn't installed"
		default:
			// Removing the download stops its scan (see stopDownloadScan):
			// the antivirus holds the file while scanning, so deleting it
			// would otherwise wait for a scan nobody needs any more.
			sctx, cancel := context.WithCancel(ctx)
			a.mu.Lock()
			if a.dlScanStop == nil {
				a.dlScanStop = map[string]context.CancelFunc{}
			}
			a.dlScanStop[s.ID] = cancel
			a.mu.Unlock()
			ok := a.runScan(sctx, "dl:"+s.ID, p, false, rec)
			a.mu.Lock()
			delete(a.dlScanStop, s.ID)
			a.mu.Unlock()
			cancel()
			if !ok {
				return // quitting: scanned at the next start
			}
			if _, err := os.Stat(p); err != nil && len(rec.Threats) > 0 {
				meta.Gone = true // the antivirus took it while scanning
			}
		}
	}
	meta.Scan = rec
	data, _ := json.Marshal(meta)
	m.SetMeta(s.ID, data)
	a.warnThreats(s.Name, rec)
}

// downloadStatus adds a download's scan result for the UI.
func (a *App) downloadStatus(st ddl.Status, spec ddl.Spec) DownloadStatus {
	ds := DownloadStatus{Status: st, Threats: []string{}}
	a.mu.Lock()
	scanning := a.scanning["dl:"+st.ID]
	a.mu.Unlock()
	meta := metaOf(spec)
	rec := meta.Scan
	switch {
	case scanning:
		ds.Scan = "scanning"
	case rec == nil:
	case meta.Gone:
		ds.Scan = "gone" // with the threats, if the scan found any before the file went
		if rec.Threats != nil {
			ds.Threats = rec.Threats
		}
	case len(rec.Threats) > 0:
		ds.Scan, ds.Threats = "threat", rec.Threats
	case rec.Error != "" && !rec.Clean:
		ds.Scan = "error"
		if rec.Error == "scanning is turned off in Settings" || rec.Error == rec.Engine+" isn't installed" {
			ds.Scan = "off"
		}
	case rec.Clean:
		ds.Scan = "clean"
	}
	if rec != nil {
		ds.ScanEngine, ds.ScanError = rec.Engine, rec.Error
	}
	return ds
}

// stopDownloadScan ends a download's malware scan if one is running.
func (a *App) stopDownloadScan(id string) {
	a.mu.Lock()
	stop := a.dlScanStop[id]
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
}
