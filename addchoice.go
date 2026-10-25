package main

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"sharkbite/internal/paste"
	"sharkbite/internal/search"
)

// "Ask where to save each download" (Settings.AskSaveLocation): the window
// shows a dialog before adding anything, and passes the user's choice to the
// ...To versions of the add functions below. Torrents added from outside the
// app (a browser's magnet link) already wait in a banner, which gets the
// same folder choice (StartTorrentIn).

// AddChoice is where to save what's being added and whether to start it.
type AddChoice struct {
	Dir    string `json:"dir"` // "" = the default download folder
	Paused bool   `json:"paused"`
}

const maxRecentDirs = 5

// dirFor is the folder a choice means.
func (a *App) dirFor(c AddChoice) string {
	if c.Dir != "" {
		return filepath.Clean(c.Dir)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings.DownloadDir
}

func sameDir(x, y string) bool {
	x, y = filepath.Clean(x), filepath.Clean(y)
	if filepath.Separator == '\\' {
		return strings.EqualFold(x, y)
	}
	return x == y
}

// noteRecentDir remembers a folder other than the default for the dialog.
func (a *App) noteRecentDir(dir string) {
	a.mu.Lock()
	if dir == "" || sameDir(dir, a.settings.DownloadDir) {
		a.mu.Unlock()
		return
	}
	list := []string{dir}
	for _, d := range a.settings.RecentDirs {
		if !sameDir(d, dir) && len(list) < maxRecentDirs {
			list = append(list, d)
		}
	}
	changed := len(a.settings.RecentDirs) == 0 || !sameDir(a.settings.RecentDirs[0], dir) || len(list) != len(a.settings.RecentDirs)
	a.settings.RecentDirs = list
	a.mu.Unlock()
	if changed {
		a.writeSettings()
	}
}

// SetAskSaveLocation turns the dialog on or off (its "Don't ask again").
func (a *App) SetAskSaveLocation(on bool) error {
	a.mu.Lock()
	a.settings.AskSaveLocation = on
	a.mu.Unlock()
	return a.writeSettings()
}

func (a *App) AddMagnetTo(link string, c AddChoice) error {
	link = strings.TrimSpace(link)
	if !strings.HasPrefix(link, "magnet:") {
		return errors.New("not a magnet link")
	}
	return a.addSourceTo(link, false, c)
}

// ChooseTorrentFiles shows the file dialog for .torrent files and returns
// what was picked (nothing if cancelled), for the window to ask about.
func (a *App) ChooseTorrentFiles() ([]string, error) {
	paths, err := runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "Add torrent files",
		Filters: []runtime.FileFilter{{DisplayName: "Torrent files (*.torrent)", Pattern: "*.torrent"}},
	})
	if paths == nil {
		paths = []string{}
	}
	return paths, err
}

func (a *App) AddTorrentFiles(paths []string, c AddChoice) error {
	for _, p := range paths {
		if err := a.addSourceTo(p, false, c); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) AddSearchResultTo(r search.Result, c AddChoice) error {
	return a.addSearchResult(r, c)
}

func (a *App) AddLinksTo(links []paste.Link, c AddChoice) (AddLinksResult, error) {
	return a.addLinks(links, c)
}

// StartTorrentIn starts a torrent waiting in the "added from outside the
// app" banner, in the folder chosen there.
func (a *App) StartTorrentIn(hash, dir string) error {
	if dir != "" {
		a.mu.Lock()
		if s := a.pendingFor(hash); s != nil {
			s.Dir = filepath.Clean(dir)
		}
		a.mu.Unlock()
	}
	if err := a.startTorrent(hash, true); err != nil {
		return err
	}
	a.noteRecentDir(dir)
	return nil
}

// askAdd hands files dropped on the window to the dialog instead of adding
// them at once, when the user wants to be asked.
func (a *App) askAdd(paths []string) bool {
	a.mu.Lock()
	ask := a.settings.AskSaveLocation
	a.mu.Unlock()
	if !ask || len(paths) == 0 || a.ctx == nil {
		return false
	}
	runtime.EventsEmit(a.ctx, "ask-add", paths)
	return true
}
