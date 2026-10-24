// Package paste turns whatever the user pastes (a block of text, a web page's
// address or an encrypted PrivateBin paste) into the list of links in it, so
// the app can offer them as downloads.
package paste

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

const maxPage = 8 << 20 // pages and pastes larger than this aren't link lists

// UserAgent is sent with page requests: many sites refuse Go's default one.
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

type Kind string

const (
	Magnet  Kind = "magnet"  // magnet:?xt=urn:btih:…
	Torrent Kind = "torrent" // a .torrent file
	File    Kind = "file"    // the address looks like a file (by its name)
	Page    Kind = "page"    // anything else: a web page, maybe a file host's
)

type Link struct {
	URL  string `json:"url"`
	Host string `json:"host"` // lower case, without "www."; empty for magnets
	Name string `json:"name"` // file name from the address, if it has one
	Kind Kind   `json:"kind"`
}

// Read returns the text behind input: a PrivateBin paste is decrypted, any
// other http(s) address is fetched, and anything else is the text itself.
// hc should go through the VPN.
func Read(ctx context.Context, hc *http.Client, input string) (string, error) {
	input = strings.TrimSpace(input)
	u, err := url.Parse(input)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(input, " \t\r\n") {
		return input, nil
	}
	if looksLikePrivateBin(u) {
		return readPrivateBin(ctx, hc, u)
	}
	if l, ok := classify(input); ok && l.Kind != Page {
		return input, nil // a link to a file (or a .torrent) is itself the list
	}
	text, err := fetchPage(ctx, hc, u)
	if errors.Is(err, errNotPage) {
		return input, nil // it was a file after all, without a telling name
	}
	return text, err
}

var errNotPage = errors.New("not a page")

func fetchPage(ctx context.Context, hc *http.Client, u *url.URL) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("User-Agent", UserAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", u.Host, resp.StatusCode)
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.HasPrefix(ct, "text/") && ct != "application/xhtml+xml" && ct != "application/json" {
		return "", errNotPage
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

var (
	urlRe    = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>\x60]+`)
	magnetRe = regexp.MustCompile(`(?i)\bmagnet:\?[^\s"'<>\x60]+`)
)

// Links finds the links in text (plain text or HTML) in the order they
// appear, without duplicates. Relative links in HTML aren't resolved: a page
// of downloads lists full addresses.
func Links(text string) []Link {
	text = html.UnescapeString(text) // &amp; in hrefs
	type hit struct {
		at int
		s  string
	}
	var hits []hit
	for _, re := range []*regexp.Regexp{urlRe, magnetRe} {
		for _, m := range re.FindAllStringIndex(text, -1) {
			hits = append(hits, hit{m[0], text[m[0]:m[1]]})
		}
	}
	slices.SortFunc(hits, func(a, b hit) int { return a.at - b.at })
	var out []Link
	seen := map[string]bool{}
	for _, h := range hits {
		l, ok := classify(trimPunct(h.s))
		if ok && !seen[l.URL] {
			seen[l.URL] = true
			out = append(out, l)
		}
	}
	return out
}

// trimPunct drops what plain text puts after a link: "see https://x.org/a."
// or "(https://x.org/a)". A ")" stays if the link has its own "(".
func trimPunct(s string) string {
	for s != "" {
		c := s[len(s)-1]
		switch {
		case strings.IndexByte(".,;:!?'\"", c) >= 0:
		case c == ')' && strings.Count(s, "(") < strings.Count(s, ")"):
		case c == ']' && strings.Count(s, "[") < strings.Count(s, "]"):
		default:
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}

// File extensions that make an address a file rather than a page.
var fileExts = map[string]bool{
	".rar": true, ".zip": true, ".7z": true, ".tar": true, ".gz": true, ".xz": true, ".zst": true, ".bz2": true,
	".iso": true, ".img": true, ".bin": true, ".exe": true, ".msi": true, ".dmg": true, ".pkg": true,
	".deb": true, ".rpm": true, ".appimage": true, ".apk": true,
	".mkv": true, ".mp4": true, ".avi": true, ".mov": true, ".webm": true,
	".mp3": true, ".flac": true, ".m4a": true, ".ogg": true, ".wav": true,
	".pdf": true, ".epub": true, ".cbz": true, ".cbr": true,
}

func classify(raw string) (Link, bool) {
	if strings.HasPrefix(strings.ToLower(raw), "magnet:") {
		if !strings.Contains(strings.ToLower(raw), "xt=urn:bt") {
			return Link{}, false
		}
		return Link{URL: raw, Kind: Magnet}, true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Link{}, false
	}
	l := Link{URL: raw, Host: strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."), Kind: Page}
	name := path.Base(u.Path)
	if name == "/" || name == "." {
		name = ""
	}
	// "part1.rar" or "game.part01.rar"; split archives ".001" too
	ext := strings.ToLower(path.Ext(name))
	switch {
	case ext == ".torrent":
		l.Kind = Torrent
	case fileExts[ext] || isSplitPart(ext):
		l.Kind = File
	}
	if l.Kind != Page {
		if n, err := url.PathUnescape(name); err == nil {
			name = n
		}
		l.Name = name
	}
	return l, true
}

// isSplitPart is true for ".001", ".002"…
func isSplitPart(ext string) bool {
	if len(ext) != 4 {
		return false
	}
	for _, c := range ext[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
