//go:build windows || (linux && cgo)

package browser

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// The window runs in a process of its own (the app started again with
// HelperArg), so nothing that goes wrong in WebView2 can take the app down:
// go-webview2 calls os.Exit on some errors and dereferences nil after others.
// The two talk in JSON lines over the helper's stdin and stdout.

// HelperArg starts the app as the window's process (see ServeHelper).
const HelperArg = "--browser-window"

type helperConfig struct {
	Proxy, DataDir, Title string
	Args                  []string
}

// command is the app → window message; event is window → app.
type command struct {
	Cmd string `json:"cmd"` // navigate, eval, query, title, settitle, show, close
	Arg string `json:"arg,omitempty"`
	ID  int64  `json:"id,omitempty"`
}

type event struct {
	Ev          string `json:"ev"` // ready, error, loaded, download, popup, title, result
	URL         string `json:"url,omitempty"`
	Disposition string `json:"disposition,omitempty"`
	Text        string `json:"text,omitempty"`
	ID          int64  `json:"id,omitempty"`
}

// ServeHelper is the window's process: it reads its configuration and then
// commands from in, opens the window, and reports to out until the window
// closes or in ends (the app quit).
func ServeHelper(in io.Reader, out io.Writer) error {
	dec := json.NewDecoder(in)
	var cfg helperConfig
	if err := dec.Decode(&cfg); err != nil {
		return fmt.Errorf("browser window: no configuration: %w", err)
	}
	var mu sync.Mutex
	enc := json.NewEncoder(out)
	send := func(e event) {
		mu.Lock()
		enc.Encode(e)
		mu.Unlock()
	}
	w, err := Open(Options{
		Proxy: cfg.Proxy, DataDir: cfg.DataDir, Title: cfg.Title, Args: cfg.Args,
		OnLoaded: func(url string) { send(event{Ev: "loaded", URL: url}) },
		OnPopup:  func(nw NewWindow) { send(event{Ev: "popup", URL: nw.URL}) },
		OnDownload: func(d Download) DownloadChoice {
			send(event{Ev: "download", URL: d.URL, Disposition: d.Disposition})
			return DownloadChoice{Cancel: true} // the app downloads it itself
		},
	})
	if err != nil {
		send(event{Ev: "error", Text: err.Error()})
		return err
	}
	send(event{Ev: "ready"})
	go func() {
		defer w.Close() // the app went away (or said so): close
		for {
			var c command
			if err := dec.Decode(&c); err != nil {
				return
			}
			switch c.Cmd {
			case "navigate":
				w.Navigate(c.Arg)
			case "eval":
				w.Eval(c.Arg)
			case "title":
				go func(id int64) { send(event{Ev: "title", ID: id, Text: w.DocumentTitle()}) }(c.ID)
			case "query":
				go func(id int64, js string) { send(event{Ev: "result", ID: id, Text: w.Query(js)}) }(c.ID, c.Arg)
			case "settitle":
				w.SetTitle(c.Arg)
			case "show":
				w.Show()
			case "close":
				return
			}
		}
	}()
	<-w.Done()
	return nil
}

// Remote is the app's handle on a window running in its own process.
type Remote struct {
	o      Options
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex // guards writes to stdin and titles
	enc    *json.Encoder
	titles map[int64]chan string // answers waited for (titles, query results), by ID
	nextID atomic.Int64
	done   chan struct{}
}

// Start runs exe (the app itself) as the window's process and returns once
// the window is open. Options' callbacks run on a goroutine of the app's.
func Start(exe string, o Options) (*Remote, error) {
	cmd := exec.Command(exe, HelperArg)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("couldn't start the browser window: %w", err)
	}
	r := &Remote{o: o, cmd: cmd, stdin: stdin, enc: json.NewEncoder(stdin), titles: map[int64]chan string{}, done: make(chan struct{})}
	if err := r.enc.Encode(helperConfig{Proxy: o.Proxy, DataDir: o.DataDir, Title: o.Title, Args: o.Args}); err != nil {
		cmd.Process.Kill()
		return nil, err
	}
	ready := make(chan error, 1)
	go r.read(bufio.NewReader(stdout), ready)
	select {
	case err := <-ready:
		if err != nil {
			r.Close()
			return nil, err
		}
	case <-time.After(30 * time.Second):
		r.Close()
		return nil, errors.New("the browser window didn't open")
	}
	return r, nil
}

// read handles the window's events until its process ends.
func (r *Remote) read(out *bufio.Reader, ready chan<- error) {
	defer func() {
		r.cmd.Wait()
		select {
		case ready <- errors.New("the browser window closed while opening (WebView2 failed)"):
		default:
		}
		r.mu.Lock()
		for id, ch := range r.titles {
			close(ch)
			delete(r.titles, id)
		}
		r.mu.Unlock()
		close(r.done)
		if r.o.OnClosed != nil {
			r.o.OnClosed()
		}
	}()
	dec := json.NewDecoder(out)
	for {
		var e event
		if err := dec.Decode(&e); err != nil {
			return
		}
		switch e.Ev {
		case "ready":
			ready <- nil
		case "error":
			ready <- errors.New(e.Text)
		case "loaded":
			if r.o.OnLoaded != nil {
				r.o.OnLoaded(e.URL)
			}
		case "download":
			if r.o.OnDownload != nil {
				r.o.OnDownload(Download{URL: e.URL, Disposition: e.Disposition, Size: -1})
			}
		case "popup":
			if r.o.OnPopup != nil {
				r.o.OnPopup(NewWindow{URL: e.URL})
			}
		case "title", "result":
			r.mu.Lock()
			ch := r.titles[e.ID]
			delete(r.titles, e.ID)
			r.mu.Unlock()
			if ch != nil {
				ch <- e.Text
			}
		}
	}
}

func (r *Remote) send(c command) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.done:
		return false
	default:
	}
	return r.enc.Encode(c) == nil
}

// Done is closed once the window (its process) is gone.
func (r *Remote) Done() <-chan struct{} { return r.done }

func (r *Remote) Navigate(url string) { r.send(command{Cmd: "navigate", Arg: url}) }
func (r *Remote) Eval(js string)      { r.send(command{Cmd: "eval", Arg: js}) }
func (r *Remote) SetTitle(s string)   { r.send(command{Cmd: "settitle", Arg: s}) }
func (r *Remote) Show()               { r.send(command{Cmd: "show"}) }

// DocumentTitle is the page's <title> ("" if the window is gone or slow).
func (r *Remote) DocumentTitle() string { return r.ask(command{Cmd: "title"}) }

// Query runs js in the page and returns its result as JSON ("" if the
// window is gone or slow).
func (r *Remote) Query(js string) string { return r.ask(command{Cmd: "query", Arg: js}) }

// ask sends c and waits for the answer with its ID.
func (r *Remote) ask(c command) string {
	id := r.nextID.Add(1)
	c.ID = id
	ch := make(chan string, 1)
	r.mu.Lock()
	r.titles[id] = ch
	r.mu.Unlock()
	if !r.send(c) {
		return ""
	}
	select {
	case t := <-ch:
		return t
	case <-time.After(10 * time.Second):
		r.mu.Lock()
		delete(r.titles, id)
		r.mu.Unlock()
		return ""
	}
}

// Close closes the window and waits for its process to end (ending it if
// it doesn't within a few seconds).
func (r *Remote) Close() {
	r.send(command{Cmd: "close"})
	r.stdin.Close()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		r.cmd.Process.Kill()
		<-r.done
	}
}
