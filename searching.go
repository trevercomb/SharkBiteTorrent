package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"sharkbite/internal/search"
	"sharkbite/internal/secret"
)

// Torrent search through the user's own Prowlarr or Jackett. The connection
// (address and API key) is stored encrypted in search.json.

type SearchStatus struct {
	Configured bool   `json:"configured"`
	Kind       string `json:"kind"` // "prowlarr" or "jackett"
	URL        string `json:"url"`
}

func (a *App) searchPath() string { return filepath.Join(a.configDir, "search.json") }

func (a *App) searchConfig() (search.Config, bool) {
	var c search.Config
	data, err := secret.ReadFile(a.searchPath())
	if err != nil || json.Unmarshal(data, &c) != nil || c.Kind == "" {
		return c, false
	}
	return c, true
}

func (a *App) saveSearchConfig(c search.Config) (SearchStatus, error) {
	data, _ := json.Marshal(c)
	if err := secret.WriteFile(a.searchPath(), data); err != nil {
		return SearchStatus{}, err
	}
	return SearchStatus{Configured: true, Kind: string(c.Kind), URL: c.URL}, nil
}

// --- Bound to the frontend ---

func (a *App) GetSearchStatus() SearchStatus {
	c, ok := a.searchConfig()
	if !ok {
		return SearchStatus{}
	}
	return SearchStatus{Configured: true, Kind: string(c.Kind), URL: c.URL}
}

// ConnectSearch checks and saves a server address and API key.
func (a *App) ConnectSearch(url, apiKey string) (SearchStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := search.Detect(ctx, url, apiKey)
	if err != nil {
		return SearchStatus{}, err
	}
	return a.saveSearchConfig(c)
}

// AutoDetectSearch finds Prowlarr or Jackett on this computer, reads its
// address and API key from its own settings, checks it and saves it.
func (a *App) AutoDetectSearch() (SearchStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	found := search.LocalServers()
	if len(found) == 0 {
		return SearchStatus{}, errors.New("didn't find Prowlarr or Jackett on this computer; enter its address and API key instead")
	}
	var lastErr error
	for _, s := range found {
		c, err := search.Detect(ctx, s.URL, s.APIKey)
		if err == nil {
			return a.saveSearchConfig(c)
		}
		lastErr = err
	}
	return SearchStatus{}, lastErr
}

func (a *App) DisconnectSearch() error {
	a.disableSearchProxy() // turn the server's proxy off while we can still reach it
	err := os.Remove(a.searchPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// SearchTorrents searches all of the user's indexers. category: "", movies,
// tv, music, software or books.
func (a *App) SearchTorrents(query, category string) ([]search.Result, error) {
	c, ok := a.searchConfig()
	if !ok {
		return nil, errors.New("search isn't set up: connect Prowlarr or Jackett in Settings")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rs, err := search.Search(ctx, c, query, category)
	if rs == nil {
		rs = []search.Result{}
	}
	return rs, err
}

// AddSearchResult downloads a search result's magnet or .torrent and adds it.
func (a *App) AddSearchResult(r search.Result) error { return a.addSearchResult(r, AddChoice{}) }

func (a *App) addSearchResult(r search.Result, choice AddChoice) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, _ := a.searchConfig()
	src, tmp, err := search.Fetch(ctx, c, r, a.searchDial)
	if err != nil {
		return err
	}
	if tmp != "" {
		defer os.Remove(tmp) // addSource keeps its own copy
	}
	return a.addSourceTo(src, false, choice)
}
