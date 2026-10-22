package main

import (
	"context"
	_ "embed"
	"html"
	"net/http"
	"regexp"
	"strings"
)

// Release notes shown in the app. They come from CHANGELOG.md: the copy
// built into the app for "What's new in this version", and the newer
// release's copy on GitHub for an available update.

//go:embed CHANGELOG.md
var changelogMD []byte

// ReleaseNote is one version's section of CHANGELOG.md. Text and Items are
// HTML-safe: escaped, with only **bold** and `code` turned into tags.
type ReleaseNote struct {
	Version string      `json:"version"`
	Date    string      `json:"date"`
	Text    []string    `json:"text"` // paragraphs before the first group
	Groups  []NoteGroup `json:"groups"`
}

type NoteGroup struct {
	Title string   `json:"title"` // Added, Changed, Fixed, Security
	Items []string `json:"items"`
}

var headingRe = regexp.MustCompile(`^## \[([^\]]+)\](?: - (\S+))?`)

// parseChangelog reads the released versions from CHANGELOG.md, newest
// first. The Unreleased section is skipped, unless unreleased names the
// pre-release it belongs to: a beta (0.3.0-beta.1) is built from what's
// committed, so its notes are the Unreleased section at its tag.
func parseChangelog(md []byte, unreleased string) []ReleaseNote {
	var out []ReleaseNote
	var cur *ReleaseNote
	var group *NoteGroup
	for _, line := range strings.Split(strings.ReplaceAll(string(md), "\r\n", "\n"), "\n") {
		if m := headingRe.FindStringSubmatch(line); m != nil {
			cur, group = nil, nil
			switch {
			case !strings.EqualFold(m[1], "Unreleased"):
				out = append(out, ReleaseNote{Version: m[1], Date: m[2]})
			case unreleased != "":
				out = append(out, ReleaseNote{Version: unreleased})
			default:
				continue
			}
			cur = &out[len(out)-1]
			continue
		}
		if cur == nil {
			continue
		}
		switch t := strings.TrimSpace(line); {
		case strings.HasPrefix(t, "### "):
			cur.Groups = append(cur.Groups, NoteGroup{Title: strings.TrimPrefix(t, "### ")})
			group = &cur.Groups[len(cur.Groups)-1]
		case strings.HasPrefix(t, "- "):
			item := inlineHTML(strings.TrimPrefix(t, "- "))
			if group == nil {
				cur.Text = append(cur.Text, item)
			} else {
				group.Items = append(group.Items, item)
			}
		case t != "":
			cur.Text = append(cur.Text, inlineHTML(t))
		}
	}
	return out
}

var (
	boldRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	codeRe = regexp.MustCompile("`([^`]+)`")
)

// inlineHTML escapes s and turns **bold** and `code` into tags; nothing else
// from the text can become markup.
func inlineHTML(s string) string {
	s = html.EscapeString(s)
	s = codeRe.ReplaceAllString(s, "<code>$1</code>")
	return boldRe.ReplaceAllString(s, "<b>$1</b>")
}

// notesBetween returns the notes for versions after `after`, up to and
// including `upTo`, newest first.
func notesBetween(notes []ReleaseNote, after, upTo string) []ReleaseNote {
	var out []ReleaseNote
	for _, n := range notes {
		if newerVersion(n.Version, after) && !newerVersion(n.Version, upTo) {
			out = append(out, n)
		}
	}
	return out
}

// updateNotes fetches the changelog of the release being offered and returns
// what changed since the running version. Best effort: nil on any error.
func updateNotes(ctx context.Context, hc *http.Client, current, latest string) []ReleaseNote {
	url := "https://raw.githubusercontent.com/trevercomb/SharkBiteTorrent/v" + latest + "/CHANGELOG.md"
	md, err := fetchSmall(ctx, hc, url)
	if err != nil {
		return nil
	}
	return notesBetween(parseChangelog(md, prereleaseOf(latest)), current, latest)
}

// --- Bound to the frontend ---

// GetReleaseNotes returns the notes for the running version (from the copy
// built into the app), for "What's new" in Settings → About.
func (a *App) GetReleaseNotes() []ReleaseNote {
	v := a.currentVersion()
	for _, n := range parseChangelog(changelogMD, prereleaseOf(v)) {
		if n.Version == v {
			return []ReleaseNote{n}
		}
	}
	return nil
}

// prereleaseOf returns v if it's a pre-release version, else "".
func prereleaseOf(v string) string {
	if strings.Contains(v, "-") {
		return v
	}
	return ""
}
