package ddl

import (
	"bytes"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testData(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// rangeServer serves data with byte ranges; delay slows each response's body
// so a test can stop a download halfway.
func rangeServer(t *testing.T, data []byte, delay *atomic.Int64) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="test file.bin"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		if delay != nil && delay.Load() > 0 {
			w = &slowWriter{ResponseWriter: w, delay: time.Duration(delay.Load())}
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type slowWriter struct {
	http.ResponseWriter
	delay time.Duration
}

func (s *slowWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		k := min(len(p), 16<<10)
		time.Sleep(s.delay)
		w, err := s.ResponseWriter.Write(p[:k])
		n += w
		if err != nil {
			return n, err
		}
		p = p[k:]
	}
	return n, nil
}

func newManager(changed *atomic.Int64) *Manager {
	return &Manager{
		Client:      func() (*http.Client, error) { return http.DefaultClient, nil },
		Connections: 4,
		OnChange: func() {
			if changed != nil {
				changed.Add(1)
			}
		},
	}
}

func waitState(t *testing.T, m *Manager, id string, want State) Status {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range m.Statuses() {
			if s.ID == id && s.State == want {
				return s
			}
			if s.ID == id && (s.State == Failed && want != Failed) {
				t.Fatalf("download failed: %s", s.Error)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("download %s never reached %s: %+v", id, want, m.Statuses())
	return Status{}
}

func checkFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: %d bytes, content differs (want %d)", path, len(got), len(want))
	}
}

func TestSegmented(t *testing.T) {
	defer func(v int64) { minSegment = v }(minSegment)
	minSegment = 64 << 10
	data := testData(t, 1<<20+123)
	srv := rangeServer(t, data, nil)
	dir := t.TempDir()
	m := newManager(nil)
	if err := m.Add(Spec{ID: "a", URL: srv.URL + "/x", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, "a", Done)
	spec := m.Specs()[0]
	if len(spec.Segments) != 4 || !spec.Ranges || spec.Size != int64(len(data)) {
		t.Errorf("segments %d, ranges %v, size %d", len(spec.Segments), spec.Ranges, spec.Size)
	}
	if spec.Name != "test file.bin" {
		t.Errorf("name %q", spec.Name)
	}
	checkFile(t, filepath.Join(dir, "test file.bin"), data)
	if _, err := os.Stat(filepath.Join(dir, "test file.bin"+PartSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Error("part file left behind")
	}
}

func TestNoRanges(t *testing.T) {
	data := testData(t, 300<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data) // ignores Range, no length either
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	dir := t.TempDir()
	m := newManager(nil)
	m.Add(Spec{ID: "a", URL: srv.URL + "/files/plain.iso", Dir: dir})
	waitState(t, m, "a", Done)
	spec := m.Specs()[0]
	if len(spec.Segments) != 1 || spec.Ranges {
		t.Errorf("segments %d, ranges %v", len(spec.Segments), spec.Ranges)
	}
	checkFile(t, filepath.Join(dir, "plain.iso"), data)
}

// A download stopped halfway and added again (as after a restart) continues
// from what was saved and ends with the right file.
func TestResume(t *testing.T) {
	defer func(v int64) { minSegment = v }(minSegment)
	minSegment = 64 << 10
	data := testData(t, 2<<20)
	var delay atomic.Int64
	delay.Store(int64(20 * time.Millisecond))
	srv := rangeServer(t, data, &delay)
	dir := t.TempDir()

	m := newManager(nil)
	m.Add(Spec{ID: "a", URL: srv.URL + "/x", Dir: dir, Name: "r.bin"})
	deadline := time.Now().Add(20 * time.Second)
	for {
		if s := m.Statuses()[0]; s.Have > int64(len(data))/4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no progress")
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.Close() // syncs and records what's on disk
	saved := m.Specs()[0]
	var have int64
	for _, s := range saved.Segments {
		have += s.Pos - s.Start
	}
	if have == 0 || have >= int64(len(data)) {
		t.Fatalf("saved progress %d of %d", have, len(data))
	}

	delay.Store(0)
	m2 := newManager(nil)
	m2.Add(saved)
	waitState(t, m2, "a", Done)
	checkFile(t, filepath.Join(dir, "r.bin"), data)
}

func TestPauseResume(t *testing.T) {
	defer func(v int64) { minSegment = v }(minSegment)
	minSegment = 64 << 10
	data := testData(t, 1<<20)
	var delay atomic.Int64
	delay.Store(int64(20 * time.Millisecond))
	srv := rangeServer(t, data, &delay)
	dir := t.TempDir()
	m := newManager(nil)
	m.Add(Spec{ID: "a", URL: srv.URL + "/x", Dir: dir})
	time.Sleep(100 * time.Millisecond)
	m.Pause("a")
	if s := m.Statuses()[0]; s.State != Paused || !m.Specs()[0].Paused {
		t.Fatalf("after Pause: %+v", s)
	}
	delay.Store(0)
	m.Resume("a")
	waitState(t, m, "a", Done)
	checkFile(t, filepath.Join(dir, "test file.bin"), data)
}

// A link that stops working (signed links expire) waits for a fresh one from
// the file host's page, then continues in the same file.
func TestExpiredLink(t *testing.T) {
	defer func(v int64) { minSegment = v }(minSegment)
	minSegment = 64 << 10
	data := testData(t, 1<<20)
	var delay atomic.Int64
	delay.Store(int64(20 * time.Millisecond))
	var oldWorks atomic.Bool
	oldWorks.Store(true)
	inner := rangeServer(t, data, &delay)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/old") && !oldWorks.Load() {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/page-instead" {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>log in</html>"))
			return
		}
		r.URL.Host, r.URL.Scheme = strings.TrimPrefix(inner.URL, "http://"), "http"
		resp, err := http.DefaultTransport.RoundTrip(r.Clone(r.Context()))
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 16<<10)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 && !oldWorks.Load() && strings.HasPrefix(r.URL.Path, "/old") {
				return // the link expires mid-download: cut the connection
			}
			w.Write(buf[:n])
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := newManager(nil)
	m.Add(Spec{ID: "a", URL: srv.URL + "/old", Page: "https://host.example/f/1", Dir: dir, Name: "e.bin"})
	time.Sleep(150 * time.Millisecond)
	oldWorks.Store(false)
	st := waitState(t, m, "a", NeedsLink)
	if st.Have == 0 || !m.Specs()[0].NeedLink {
		t.Errorf("waiting for a link: %+v", st)
	}

	delay.Store(0)
	m.SetURL("a", srv.URL+"/new")
	waitState(t, m, "a", Done)
	checkFile(t, filepath.Join(dir, "e.bin"), data)

	// A link that answers with a web page is an expired link, too.
	m.Add(Spec{ID: "b", URL: srv.URL + "/page-instead", Page: "https://host.example/f/2", Dir: dir, Name: "p.bin"})
	waitState(t, m, "b", NeedsLink)
	// Without a page to get a new link from, it simply fails.
	m.Add(Spec{ID: "c", URL: srv.URL + "/page-instead", Dir: dir, Name: "q.bin"})
	waitState(t, m, "c", Failed)
}

func TestNames(t *testing.T) {
	data := testData(t, 1000)
	srv := rangeServer(t, data, nil)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test file.bin"), []byte("someone else's"), 0o644)
	m := newManager(nil)
	m.Add(Spec{ID: "a", URL: srv.URL + "/x", Dir: dir})
	waitState(t, m, "a", Done)
	if n := m.Specs()[0].Name; n != "test file (2).bin" {
		t.Errorf("name %q", n)
	}
	checkFile(t, filepath.Join(dir, "test file (2).bin"), data)

	// Two unfinished downloads never share a name (they'd share a part file).
	var delay atomic.Int64
	delay.Store(int64(5 * time.Millisecond))
	slow := rangeServer(t, testData(t, 200<<10), &delay)
	m.Add(Spec{ID: "b", URL: slow.URL + "/x", Dir: dir, Name: "same.rar"})
	m.Add(Spec{ID: "c", URL: slow.URL + "/x", Dir: dir, Name: "same.rar"})
	if b, c := m.Specs()[1].Name, m.Specs()[2].Name; b == c {
		t.Errorf("both named %q", b)
	}
	m.Close()

	for in, want := range map[string]string{
		"../../evil.exe":    "evil.exe",
		`C:\x\y.zip`:        "y.zip",
		"a<b>c:d.rar":       "a_b_c_d.rar",
		"CON.txt":           "_CON.txt",
		"name. ":            "name",
		"..":                "",
		"part1.rar\x00.txt": "part1.rar_.txt",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContentRange(t *testing.T) {
	for h, want := range map[string][2]int64{
		"bytes 0-0/2097152000": {0, 2097152000},
		"bytes 500-999/1000":   {500, 1000},
		"bytes */1000":         {-1, -1},
		"bytes 0-1/*":          {-1, -1},
		"items 0-1/2":          {-1, -1},
	} {
		s, tot, ok := contentRange(h)
		if want[0] < 0 {
			if ok {
				t.Errorf("%q accepted", h)
			}
			continue
		}
		if !ok || s != want[0] || tot != want[1] {
			t.Errorf("%q: %d %d %v", h, s, tot, ok)
		}
	}
}

// While there's no connection (the VPN is down), downloads wait as long as it
// takes instead of failing, and finish once it's back.
func TestOffline(t *testing.T) {
	defer func(v time.Duration) { offlineWait = v }(offlineWait)
	offlineWait = 10 * time.Millisecond
	data := testData(t, 100<<10)
	srv := rangeServer(t, data, nil)
	var down atomic.Bool
	down.Store(true)
	m := newManager(nil)
	m.Client = func() (*http.Client, error) {
		if down.Load() {
			return nil, errors.New("the VPN isn't connected")
		}
		return http.DefaultClient, nil
	}
	dir := t.TempDir()
	m.Add(Spec{ID: "a", URL: srv.URL + "/x", Dir: dir, Name: "o.bin"})
	time.Sleep(time.Duration(maxRetries*3) * offlineWait) // longer than the retries would last
	if s := m.Statuses()[0]; s.State != Running || !s.Offline {
		t.Fatalf("while offline: %+v", s)
	}
	down.Store(false)
	waitState(t, m, "a", Done)
	checkFile(t, filepath.Join(dir, "o.bin"), data)
}

// A connection that stops sending without closing is dropped and retried.
func TestStall(t *testing.T) {
	defer func(v time.Duration) { stallTimeout = v }(stallTimeout)
	stallTimeout = 200 * time.Millisecond
	data := testData(t, 200<<10)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 { // the first real request (after the probe) hangs halfway
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data[:len(data)/2])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	dir := t.TempDir()
	m := newManager(nil)
	m.Add(Spec{ID: "a", URL: srv.URL + "/x", Dir: dir, Name: "s.bin"})
	waitState(t, m, "a", Done)
	checkFile(t, filepath.Join(dir, "s.bin"), data)
	if calls.Load() < 3 {
		t.Errorf("only %d requests: the stalled one wasn't retried", calls.Load())
	}
}

// A saved download whose name has a folder in it (a tampered or very old
// downloads.json) fails instead of writing outside its folder.
func TestUnsafeSavedName(t *testing.T) {
	data := testData(t, 1000)
	srv := rangeServer(t, data, nil)
	dir := t.TempDir()
	m := newManager(nil)
	m.Add(Spec{ID: "a", URL: srv.URL + "/x", Dir: dir, Name: `..\..\evil.exe`, Size: 1000, Ranges: true,
		Segments: []Segment{{Start: 0, End: 1000}}})
	st := waitState(t, m, "a", Failed)
	if !strings.Contains(st.Error, "usable file name") {
		t.Errorf("error %q", st.Error)
	}
	for _, n := range []string{"ok.iso", "part1.rar"} {
		if !plainName(n) {
			t.Errorf("%q refused", n)
		}
	}
	for _, n := range []string{"", "..", "../x", `a\b`, "/etc/passwd", `C:\x`} {
		if plainName(n) {
			t.Errorf("%q accepted", n)
		}
	}
}
