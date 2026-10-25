// Package ddl downloads files over HTTP(S) ("direct downloads"), through
// whatever client the app gives it (the VPN's). A file is fetched in
// segments over several connections when the server allows byte ranges, and
// resumes where it stopped after a restart or a dropped connection. Progress
// is only recorded once the data is on disk, so a crash can lose a little
// work but never marks missing bytes as done.
package ddl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

type State string

const (
	Queued    State = "queued"
	Running   State = "running"
	Paused    State = "paused"
	NeedsLink State = "needsLink" // the link stopped working; a fresh one from Page continues it
	Done      State = "done"
	Failed    State = "failed"
)

// PartSuffix is added to a file's name while it downloads.
const PartSuffix = ".sbtpart"

// Segment is the byte range [Start, End) of the file; bytes before Pos are on
// disk. End is -1 while the size is unknown (one segment, no ranges).
type Segment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Pos   int64 `json:"pos"`
}

// Spec is what the app saves to resume a download.
type Spec struct {
	ID       string    `json:"id"`
	URL      string    `json:"url"`            // the file's current link
	Page     string    `json:"page,omitempty"` // the file host's page the link came from
	Dir      string    `json:"dir"`
	Name     string    `json:"name"`   // final file name; from the server if empty
	Size     int64     `json:"size"`   // -1 if unknown
	Ranges   bool      `json:"ranges"` // the server accepts byte ranges
	Segments []Segment `json:"segments,omitempty"`
	Paused   bool      `json:"paused,omitempty"`
	Done     bool      `json:"done,omitempty"`
	Error    string    `json:"error,omitempty"` // why it failed or needs a link
	NeedLink bool      `json:"needLink,omitempty"`
	// Meta is the app's own data about the download (its malware scan),
	// kept and saved with it but never looked at here.
	Meta json.RawMessage `json:"meta,omitempty"`
}

type Status struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Dir   string `json:"dir"`
	Page  string `json:"page"`
	Size  int64  `json:"size"`
	Have  int64  `json:"have"`
	Rate  int64  `json:"rate"` // bytes per second
	State State  `json:"state"`
	Error string `json:"error"`
	// Offline: running, but waiting for a connection (the VPN is down).
	Offline bool `json:"offline"`
}

// ErrLinkExpired means the server refused a link that worked before (or
// answered with a web page): a fresh link from the file host is needed.
var ErrLinkExpired = errors.New("the download link stopped working")

// ErrOffline wraps an error from Manager.Client: there's no connection (the
// VPN is down), so downloads wait instead of failing.
var ErrOffline = errors.New("no connection")

type Manager struct {
	// Client returns the HTTP client to use now (the VPN's); an error means
	// there's no connection, and downloads wait.
	Client func() (*http.Client, error)
	// Limiter, if set, caps the total download speed.
	Limiter *rate.Limiter
	// OnChange is called (from any goroutine) when something worth saving
	// changed: the app saves Specs().
	OnChange func()
	// Connections per file, when the server accepts ranges (default 4).
	Connections int
	// UserAgent sent with requests; some hosts refuse Go's default one.
	UserAgent string

	mu   sync.Mutex
	jobs map[string]*job
	ord  []string
}

type job struct {
	spec   Spec // guarded by Manager.mu; Segments[i].Pos is the synced position
	state  State
	cancel context.CancelFunc
	done   chan struct{}

	// live positions (written, maybe not yet synced), guarded by Manager.mu
	pos     []int64
	offline bool // waiting for a connection; guarded by Manager.mu

	rateAt   time.Time
	rateHave int64
	rate     int64
}

var minSegment int64 = 8 << 20 // don't split files into pieces smaller than this (tests lower it)

// offlineWait is how long a download waits before checking the connection again (tests lower it).
var offlineWait = 5 * time.Second

// stallTimeout drops a connection that has sent nothing for this long (tests lower it).
var stallTimeout = time.Minute

const (
	syncEvery    = 5 * time.Second
	bufSize      = 256 << 10
	maxRetries   = 8
	firstBackoff = 2 * time.Second
)

// Add starts (or, for a saved paused/done one, just lists) a download.
func (m *Manager) Add(s Spec) error {
	if s.ID == "" || s.URL == "" || s.Dir == "" {
		return errors.New("download needs an ID, a link and a folder")
	}
	defer m.changed() // saved right away, not only once it has made progress (after the unlock below)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs == nil {
		m.jobs = map[string]*job{}
	}
	if _, dup := m.jobs[s.ID]; dup {
		return fmt.Errorf("download %s already exists", s.ID)
	}
	if s.Size == 0 && len(s.Segments) == 0 {
		s.Size = -1
	}
	j := &job{spec: s, state: Queued}
	switch {
	case s.Done:
		j.state = Done
	case s.NeedLink:
		j.state = NeedsLink
	case s.Paused:
		j.state = Paused
	case s.Error != "":
		j.state = Failed
	}
	if s.Name != "" && !s.Done && len(s.Segments) == 0 {
		j.spec.Name = m.unusedNameLocked(j, SafeName(s.Name))
	}
	m.jobs[s.ID] = j
	m.ord = append(m.ord, s.ID)
	if j.state == Queued {
		m.startLocked(j)
	}
	return nil
}

func (m *Manager) startLocked(j *job) {
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	j.done = make(chan struct{})
	j.state = Running
	j.spec.Paused, j.spec.Error, j.spec.NeedLink = false, "", false
	go m.run(ctx, j)
}

// stopLocked cancels a running job and returns a channel closed once it has
// stopped (nil if it wasn't running).
func (m *Manager) stopLocked(j *job) chan struct{} {
	if j.cancel == nil {
		return nil
	}
	j.cancel()
	j.cancel = nil
	return j.done
}

func (m *Manager) Pause(id string) {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil || j.state == Done {
		m.mu.Unlock()
		return
	}
	done := m.stopLocked(j)
	j.state, j.spec.Paused = Paused, true
	m.mu.Unlock()
	if done != nil {
		<-done
	}
	m.changed()
}

// Resume restarts a paused or failed download (not one waiting for a link:
// that needs SetURL).
func (m *Manager) Resume(id string) {
	m.mu.Lock()
	j := m.jobs[id]
	if j != nil && (j.state == Paused || j.state == Failed) {
		m.startLocked(j)
	}
	m.mu.Unlock()
	m.changed()
}

// SetURL gives a download a fresh link (from its file host's page) and
// continues it.
func (m *Manager) SetURL(id, url string) {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil || j.state == Done {
		m.mu.Unlock()
		return
	}
	done := m.stopLocked(j)
	j.spec.URL = url
	m.mu.Unlock()
	if done != nil {
		<-done
	}
	m.mu.Lock()
	if m.jobs[id] == j {
		m.startLocked(j)
	}
	m.mu.Unlock()
	m.changed()
}

// SetMeta replaces a download's Meta (see Spec).
func (m *Manager) SetMeta(id string, meta json.RawMessage) {
	m.mu.Lock()
	j := m.jobs[id]
	if j != nil {
		j.spec.Meta = meta
	}
	m.mu.Unlock()
	if j != nil {
		m.changed()
	}
}

// Remove stops a download and forgets it; with deleteFile, the partial (or
// finished) file goes too.
func (m *Manager) Remove(id string, deleteFile bool) error {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil {
		m.mu.Unlock()
		return nil
	}
	done := m.stopLocked(j)
	delete(m.jobs, id)
	for i, x := range m.ord {
		if x == id {
			m.ord = append(m.ord[:i:i], m.ord[i+1:]...)
			break
		}
	}
	spec := j.spec
	m.mu.Unlock()
	if done != nil {
		<-done
	}
	m.changed()
	if !deleteFile || spec.Name == "" {
		return nil
	}
	p := partPath(spec)
	if spec.Done {
		p = filepath.Join(spec.Dir, spec.Name)
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Close stops every download (their progress so far is kept).
func (m *Manager) Close() {
	m.mu.Lock()
	var waits []chan struct{}
	for _, j := range m.jobs {
		if d := m.stopLocked(j); d != nil {
			waits = append(waits, d)
			j.state = Queued
		}
	}
	m.mu.Unlock()
	for _, d := range waits {
		<-d
	}
}

func (m *Manager) Specs() []Spec {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Spec, 0, len(m.ord))
	for _, id := range m.ord {
		s := m.jobs[id].spec
		s.Segments = append([]Segment(nil), s.Segments...)
		out = append(out, s)
	}
	return out
}

func (m *Manager) Statuses() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]Status, 0, len(m.ord))
	for _, id := range m.ord {
		j := m.jobs[id]
		have := j.have()
		if j.state != Running {
			j.rate, j.rateAt = 0, time.Time{}
		} else if dt := now.Sub(j.rateAt); j.rateAt.IsZero() || dt >= time.Second {
			if !j.rateAt.IsZero() {
				// smoothed over the calls (the UI polls about once a second)
				r := int64(float64(have-j.rateHave) / dt.Seconds())
				j.rate = (j.rate + 2*r) / 3
			}
			j.rateAt, j.rateHave = now, have
		}
		out = append(out, Status{ID: id, Name: j.spec.Name, Dir: j.spec.Dir, Page: j.spec.Page,
			Size: j.spec.Size, Have: have, Rate: j.rate, State: j.state, Error: j.spec.Error,
			Offline: j.state == Running && j.offline})
	}
	return out
}

// have counts bytes written, synced or not. Caller holds Manager.mu.
func (j *job) have() int64 {
	var n int64
	for i, s := range j.spec.Segments {
		p := s.Pos
		if i < len(j.pos) && j.pos[i] > p {
			p = j.pos[i]
		}
		n += p - s.Start
	}
	return n
}

func (m *Manager) changed() {
	if m.OnChange != nil {
		m.OnChange()
	}
}

func partPath(s Spec) string { return filepath.Join(s.Dir, s.Name+PartSuffix) }

// plainName is true for a file name with no folder in it: joined to the
// download folder, it stays there.
func plainName(n string) bool {
	return n != "" && filepath.IsLocal(n) && filepath.Base(n) == n && !strings.ContainsAny(n, `/\`)
}

// run downloads one file until it's done, fails, needs a link or is stopped.
func (m *Manager) run(ctx context.Context, j *job) {
	defer close(j.done)
	err := m.download(ctx, j)
	m.mu.Lock()
	switch {
	case ctx.Err() != nil:
		// stopped: Pause/Remove/Close set the state
	case err == nil:
		j.state, j.spec.Done = Done, true
	case errors.Is(err, ErrLinkExpired) && j.spec.Page != "":
		j.state, j.spec.NeedLink, j.spec.Error = NeedsLink, true, err.Error()
	default:
		j.state, j.spec.Error = Failed, err.Error()
	}
	j.cancel = nil
	m.mu.Unlock()
	m.changed()
}

func (m *Manager) download(ctx context.Context, j *job) error {
	m.mu.Lock()
	needProbe := len(j.spec.Segments) == 0 || j.spec.Name == ""
	m.mu.Unlock()
	if needProbe {
		if err := m.probe(ctx, j); err != nil {
			return err
		}
	}
	m.mu.Lock()
	spec := j.spec
	j.pos = make([]int64, len(spec.Segments))
	for i, s := range spec.Segments {
		j.pos[i] = s.Pos
	}
	m.mu.Unlock()

	// Names from servers go through SafeName, but one restored from a
	// saved download isn't; never write outside the download folder.
	if !plainName(spec.Name) {
		return fmt.Errorf("%q isn't a usable file name", spec.Name)
	}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(partPath(spec), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	// Segments write all over the file from the start: give it its full
	// size up front, without the filesystem filling the gaps with zeros.
	makeSparse(f) // best effort: on filesystems without sparse files it's just slower
	if st, err := f.Stat(); err == nil && spec.Size > 0 && st.Size() < spec.Size {
		if err := f.Truncate(spec.Size); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, len(spec.Segments))
	running := 0
	for i, s := range spec.Segments {
		if s.End >= 0 && s.Pos >= s.End {
			continue
		}
		running++
		go func(i int) { errc <- m.fetchSegment(ctx, j, f, i) }(i)
	}

	// Record progress only after syncing: what's saved is on disk.
	tick := time.NewTicker(syncEvery)
	defer tick.Stop()
	var firstErr error
	for running > 0 {
		select {
		case err := <-errc:
			running--
			if err != nil && firstErr == nil {
				firstErr = err
				cancel() // the others stop too
			}
		case <-tick.C:
			m.syncProgress(j, f)
		}
	}
	m.syncProgress(j, f)
	if firstErr != nil {
		if ctx.Err() != nil && errors.Is(firstErr, context.Canceled) {
			return ctx.Err()
		}
		return firstErr
	}
	if err := f.Close(); err != nil {
		return err
	}
	m.mu.Lock()
	spec = j.spec
	m.mu.Unlock()
	if spec.Size >= 0 {
		if st, err := os.Stat(partPath(spec)); err != nil || st.Size() != spec.Size {
			return errors.New("the downloaded file has the wrong size")
		}
	}
	final := freeName(spec.Dir, spec.Name)
	if err := os.Rename(partPath(spec), filepath.Join(spec.Dir, final)); err != nil {
		return err
	}
	m.mu.Lock()
	j.spec.Name = final
	m.mu.Unlock()
	return nil
}

func (m *Manager) syncProgress(j *job, f *os.File) {
	m.mu.Lock()
	snap := append([]int64(nil), j.pos...)
	m.mu.Unlock()
	if f.Sync() != nil {
		return
	}
	m.mu.Lock()
	moved := false
	for i := range snap {
		if i < len(j.spec.Segments) && snap[i] > j.spec.Segments[i].Pos {
			j.spec.Segments[i].Pos = snap[i]
			moved = true
		}
	}
	m.mu.Unlock()
	if moved {
		m.changed()
	}
}

func (m *Manager) request(ctx context.Context, url string, from, to int64) (*http.Response, error) {
	hc, err := m.Client()
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrOffline, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if m.UserAgent != "" {
		req.Header.Set("User-Agent", m.UserAgent)
	}
	if to >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to-1))
	} else if from > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if err := checkResponse(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// checkResponse sorts out answers that aren't the file.
func checkResponse(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("%w (HTTP %d)", ErrLinkExpired, resp.StatusCode)
	case http.StatusRequestedRangeNotSatisfiable:
		return errors.New("the server's file changed size")
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); ct == "text/html" {
		return fmt.Errorf("%w (got a web page)", ErrLinkExpired)
	}
	return nil
}

// probe asks for the first byte to learn the size, whether ranges work and
// the file's name, then plans the segments.
func (m *Manager) probe(ctx context.Context, j *job) error {
	m.mu.Lock()
	url := j.spec.URL
	m.mu.Unlock()
	resp, err := m.retry(ctx, j, func() (*http.Response, error) { return m.request(ctx, url, 0, 1) })
	if err != nil {
		return err
	}
	resp.Body.Close()
	size, ranges := int64(-1), false
	if resp.StatusCode == http.StatusPartialContent {
		if _, total, ok := contentRange(resp.Header.Get("Content-Range")); ok {
			size, ranges = total, true
		}
	} else if resp.ContentLength >= 0 {
		size = resp.ContentLength
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if j.spec.Name == "" {
		j.spec.Name = m.unusedNameLocked(j, fileName(resp))
	}
	if len(j.spec.Segments) > 0 {
		if j.spec.Size >= 0 && size >= 0 && size != j.spec.Size {
			return errors.New("the file on the server isn't the one this download started with (size changed)")
		}
		return nil
	}
	j.spec.Size, j.spec.Ranges = size, ranges
	n := m.Connections
	if n <= 0 {
		n = 4
	}
	if !ranges || size < 0 {
		n = 1
	} else if most := int(size / minSegment); n > most {
		n = max(most, 1)
	}
	if size < 0 {
		j.spec.Segments = []Segment{{Start: 0, End: -1}}
		return nil
	}
	for i := range n {
		s := size * int64(i) / int64(n)
		e := size * int64(i+1) / int64(n)
		j.spec.Segments = append(j.spec.Segments, Segment{Start: s, End: e, Pos: s})
	}
	return nil
}

// retry calls do until it succeeds, the link has expired, or it fails
// maxRetries times; it waits longer each time. While there's no connection
// it just waits.
func (m *Manager) retry(ctx context.Context, j *job, do func() (*http.Response, error)) (*http.Response, error) {
	wait := firstBackoff
	for try := 1; ; try++ {
		resp, err := do()
		if errors.Is(err, ErrOffline) {
			if err := m.waitOnline(ctx, j); err != nil {
				return nil, err
			}
			try--
			continue
		}
		m.setOffline(j, false)
		if err == nil || errors.Is(err, ErrLinkExpired) || ctx.Err() != nil || try >= maxRetries {
			return resp, err
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		wait = min(wait*2, time.Minute)
	}
}

func (m *Manager) fetchSegment(ctx context.Context, j *job, f *os.File, i int) error {
	buf := make([]byte, bufSize)
	failures := 0
	for {
		m.mu.Lock()
		seg, pos, url, ranges := j.spec.Segments[i], j.pos[i], j.spec.URL, j.spec.Ranges
		m.mu.Unlock()
		if seg.End >= 0 && pos >= seg.End {
			return nil
		}
		if pos > seg.Start && !ranges {
			// the server can't continue a file: start it again (a file
			// without ranges is always a single segment)
			pos = seg.Start
			m.mu.Lock()
			j.pos[i], j.spec.Segments[i].Pos = seg.Start, seg.Start
			m.mu.Unlock()
			if err := f.Truncate(seg.Start); err != nil {
				return err
			}
		}
		// A connection that stops sending without closing is dropped after
		// stallTimeout and tried again.
		reqCtx, cancelReq := context.WithCancel(ctx)
		var stalled atomic.Bool
		stall := time.AfterFunc(stallTimeout, func() { stalled.Store(true); cancelReq() })
		resp, err := m.request(reqCtx, url, pos, seg.End)
		if errors.Is(err, ErrOffline) {
			stall.Stop()
			cancelReq()
			if err := m.waitOnline(ctx, j); err != nil {
				return err
			}
			continue
		}
		m.setOffline(j, false)
		if err == nil {
			err = m.copySegment(reqCtx, j, f, i, resp, pos, seg.End, buf, stall)
			resp.Body.Close()
		}
		stall.Stop()
		cancelReq()
		if err != nil && ctx.Err() == nil && stalled.Load() {
			err = errors.New("the connection stalled")
		}
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrLinkExpired), ctx.Err() != nil:
			return err
		}
		m.mu.Lock()
		progressed := j.pos[i] > pos
		m.mu.Unlock()
		if progressed {
			failures = 0 // a dropped connection after real progress isn't a strike
		}
		if failures++; failures >= maxRetries {
			return err
		}
		select {
		case <-time.After(min(firstBackoff<<(failures-1), time.Minute)):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Manager) copySegment(ctx context.Context, j *job, f *os.File, i int, resp *http.Response, pos, end int64, buf []byte, stall *time.Timer) error {
	if pos > 0 || end >= 0 {
		if resp.StatusCode != http.StatusPartialContent {
			if pos > 0 {
				return errors.New("the server stopped accepting byte ranges")
			}
		} else if start, _, ok := contentRange(resp.Header.Get("Content-Range")); !ok || start != pos {
			return errors.New("the server sent the wrong part of the file")
		}
	}
	for {
		want := len(buf)
		if end >= 0 && int64(want) > end-pos {
			want = int(end - pos)
		}
		if want == 0 {
			return nil
		}
		n, rerr := resp.Body.Read(buf[:want])
		if n > 0 {
			stall.Stop() // waiting for the speed limit isn't a stall
			if m.Limiter != nil {
				if err := waitN(ctx, m.Limiter, n); err != nil {
					return err
				}
			}
			stall.Reset(stallTimeout)
			if _, err := f.WriteAt(buf[:n], pos); err != nil {
				return err
			}
			pos += int64(n)
			m.mu.Lock()
			j.pos[i] = pos
			m.mu.Unlock()
		}
		switch {
		case rerr == nil:
		case errors.Is(rerr, io.EOF):
			if end < 0 {
				return nil // unknown size: the end of the body is the end of the file
			}
			if pos < end {
				return fmt.Errorf("connection closed early: %w", io.ErrUnexpectedEOF)
			}
			return nil
		default:
			return rerr
		}
	}
}

// waitOnline waits a little while there's no connection (the VPN is down or
// reconnecting); it doesn't count as a failed attempt.
func (m *Manager) waitOnline(ctx context.Context, j *job) error {
	m.setOffline(j, true)
	select {
	case <-time.After(offlineWait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) setOffline(j *job, on bool) {
	m.mu.Lock()
	j.offline = on
	m.mu.Unlock()
}

// waitN waits for n bytes from the limiter, in pieces it can grant.
func waitN(ctx context.Context, l *rate.Limiter, n int) error {
	for n > 0 {
		k := min(n, l.Burst())
		if k <= 0 {
			return nil
		}
		if err := l.WaitN(ctx, k); err != nil {
			return err
		}
		n -= k
	}
	return nil
}

// contentRange parses "bytes start-end/total".
func contentRange(h string) (start, total int64, ok bool) {
	rest, found := strings.CutPrefix(h, "bytes ")
	if !found {
		return 0, 0, false
	}
	rng, tot, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, false
	}
	s, _, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(s, 10, 64)
	total, err2 := strconv.ParseInt(tot, 10, 64)
	if err1 != nil || err2 != nil || total <= 0 {
		return 0, 0, false
	}
	return start, total, true
}

// fileName picks the name from Content-Disposition, else the link's path.
func fileName(resp *http.Response) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		if n := SafeName(params["filename"]); n != "" {
			return n
		}
	}
	if n := SafeName(filepath.Base(resp.Request.URL.Path)); n != "" && n != "." && n != "/" {
		return n
	}
	return "download"
}

// SafeName makes a server-supplied name safe as a file name on every OS:
// no folders, no characters Windows refuses, no reserved device names.
func SafeName(n string) string {
	n = strings.TrimSpace(n)
	if i := strings.LastIndexAny(n, `/\`); i >= 0 {
		n = n[i+1:]
	}
	n = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"|?*`, r) {
			return '_'
		}
		return r
	}, n)
	n = strings.TrimRight(n, ". ")
	if n == "" || n == "." || n == ".." {
		return ""
	}
	base := strings.ToUpper(strings.SplitN(n, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		n = "_" + n
	}
	if len(n) > 200 {
		ext := filepath.Ext(n)
		if len(ext) > 20 {
			ext = ""
		}
		n = n[:200-len(ext)] + ext
	}
	return n
}

// unusedNameLocked returns name, or "name (2).ext" etc., so that no other
// unfinished download in the same folder has it (they'd share a part file)
// and no file by that name (or its part file) is already there.
func (m *Manager) unusedNameLocked(j *job, name string) string {
	taken := func(n string) bool {
		for _, o := range m.jobs {
			if o != j && !o.spec.Done && o.spec.Dir == j.spec.Dir && strings.EqualFold(o.spec.Name, n) {
				return true
			}
		}
		_, err := os.Lstat(filepath.Join(j.spec.Dir, n+PartSuffix))
		return !errors.Is(err, os.ErrNotExist) || !nameFree(j.spec.Dir, n)
	}
	return numbered(name, taken)
}

// freeName returns name, or "name (2).ext" etc. if a file by that name exists.
func freeName(dir, name string) string {
	return numbered(name, func(n string) bool { return !nameFree(dir, n) })
}

func nameFree(dir, name string) bool {
	_, err := os.Lstat(filepath.Join(dir, name))
	return errors.Is(err, os.ErrNotExist)
}

func numbered(name string, taken func(string) bool) string {
	if !taken(name) {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s (%d)%s", base, i, ext); !taken(n) {
			return n
		}
	}
}
