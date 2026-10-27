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

// HTTPError is a page that answered with something other than 200: a file
// host that shows its pages to browsers only answers 403 or 404 here.
type HTTPError struct {
	Host   string
	Status int
}

func (e *HTTPError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.Host, e.Status) }

// PageLink is input, an http(s) address, as a page (for the browser window).
func PageLink(input string) (Link, bool) {
	input = strings.TrimSpace(input)
	u, err := url.Parse(input)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Link{}, false
	}
	return Link{URL: input, Host: strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."), Kind: Page}, true
}

// Read returns the text behind input: a PrivateBin paste is decrypted, any
// other http(s) address is fetched, and anything else is the text itself.
// hc should go through the VPN.
func Read(ctx context.Context, hc *http.Client, input string) (string, error) {
	text, _, err := read(ctx, hc, input)
	return text, err
}

// Find reads input as Read does and lists the links in it. Links in a
// fetched page may be relative to it (a folder listing's "file.iso", an
// archive's "/download/item/file.zip"): they're resolved against the page.
func Find(ctx context.Context, hc *http.Client, input string) ([]Link, error) {
	text, base, err := read(ctx, hc, input)
	if err != nil {
		return nil, err
	}
	return LinksFrom(text, base), nil
}

// read is Read, also returning the address of the page it fetched (after
// redirects), nil if it fetched none.
func read(ctx context.Context, hc *http.Client, input string) (string, *url.URL, error) {
	input = strings.TrimSpace(input)
	u, err := url.Parse(input)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(input, " \t\r\n") {
		return input, nil, nil
	}
	if looksLikePrivateBin(u) {
		text, err := readPrivateBin(ctx, hc, u)
		return text, nil, err
	}
	if l, ok := classify(input); ok && l.Kind != Page {
		return input, nil, nil // a link to a file (or a .torrent) is itself the list
	}
	text, base, err := fetchPage(ctx, hc, u)
	if errors.Is(err, errNotPage) {
		return input, nil, nil // it was a file after all, without a telling name
	}
	return text, base, err
}

var errNotPage = errors.New("not a page")

func fetchPage(ctx context.Context, hc *http.Client, u *url.URL) (string, *url.URL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Accept", "text/html,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("User-Agent", UserAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, &HTTPError{Host: u.Host, Status: resp.StatusCode}
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if ct != "" && !strings.HasPrefix(ct, "text/") && ct != "application/xhtml+xml" && ct != "application/json" {
		return "", nil, errNotPage
	}
	if cd, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); cd == "attachment" {
		return "", nil, errNotPage // sent to be saved, whatever its type
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	if err != nil {
		return "", nil, err
	}
	// Plain text (not a web page) without a link in it is a file of its
	// own, such as a .txt or .nfo; with links it's a list of them.
	if ct != "" && ct != "text/html" && ct != "application/xhtml+xml" &&
		!urlRe.Match(b) && !magnetRe.Match(b) {
		return "", nil, errNotPage
	}
	return string(b), resp.Request.URL, nil
}

var (
	urlRe    = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>\x60]+`)
	magnetRe = regexp.MustCompile(`(?i)\bmagnet:\?[^\s"'<>\x60]+`)
	hrefRe   = regexp.MustCompile(`(?i)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// Links finds the full addresses and magnet links in text (plain text or
// HTML) in the order they appear, without duplicates.
func Links(text string) []Link { return LinksFrom(text, nil) }

// LinksFrom is Links for the text of the page at base: its relative links
// (href="file.iso", "../other/", "/download/x") are found too, resolved
// against base. Links that only re-sort or jump within the page ("?C=N;O=D",
// "#top") and a folder listing's link to its parent are left out.
func LinksFrom(text string, base *url.URL) []Link {
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
	if base != nil {
		for _, m := range hrefRe.FindAllStringSubmatchIndex(text, -1) {
			i := 2
			if m[2] < 0 {
				i = 4
			}
			if r, ok := resolve(base, strings.TrimSpace(text[m[i]:m[i+1]])); ok {
				hits = append(hits, hit{m[0], r})
			}
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

// resolve turns a page's relative link into a full address. Full addresses
// are left to the patterns above (found there already), and links that
// don't lead anywhere new (anchors, sort orders, the parent folder,
// javascript:, mailto:) are dropped.
func resolve(base *url.URL, href string) (string, bool) {
	if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "?") {
		return "", false
	}
	ref, err := url.Parse(href)
	if err != nil || ref.Scheme != "" || ref.Host != "" && !strings.HasPrefix(href, "//") {
		return "", false // absolute (or not a web link): not ours to resolve
	}
	u := base.ResolveReference(ref)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	u.Fragment = ""
	// A folder listing's "Parent Directory": an ancestor of the page itself.
	if u.Host == base.Host && u.RawQuery == "" && strings.HasSuffix(u.Path, "/") &&
		len(u.Path) < len(base.Path) && strings.HasPrefix(base.Path, u.Path) {
		return "", false
	}
	return u.String(), true
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
