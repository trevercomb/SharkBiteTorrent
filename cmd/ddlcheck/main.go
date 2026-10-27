// ddlcheck downloads a file with the direct-download engine through a WARP
// tunnel, stops it partway, resumes it from the saved progress (as after a
// restart), and checks the result against a SHA-256.
// Run: go run ./cmd/ddlcheck <file URL> <sha256 hex> <folder>
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sharkbite/internal/ddl"
	"sharkbite/internal/tunnel"
	"sharkbite/internal/warp"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Println("usage: ddlcheck <file URL> <sha256 hex> <folder>")
		os.Exit(2)
	}
	url, want, dir := os.Args[1], strings.ToLower(os.Args[2]), os.Args[3]
	cfgDir, _ := os.UserConfigDir()
	acc, err := warp.LoadOrRegister(filepath.Join(cfgDir, "SharkBiteTorrent", "warp.json"))
	if err != nil {
		fail("register:", err)
	}
	tun, err := tunnel.Start(tunnel.Config{PrivateKey: acc.PrivateKey, PeerPublicKey: acc.PeerPublicKey, Endpoint: acc.Endpoint,
		Addresses: []string{acc.AddressV4, acc.AddressV6}, DNS: []string{"1.1.1.1", "1.0.0.1"}})
	if err != nil {
		fail("tunnel:", err)
	}
	defer tun.Close()
	client := func() (*http.Client, error) {
		c := tun.HTTPClient()
		c.Timeout = 0 // downloads take as long as they take; stalls are caught by the engine's retries
		return c, nil
	}

	m := &ddl.Manager{Client: client, Connections: 4}
	if err := m.Add(ddl.Spec{ID: "check", URL: url, Dir: dir}); err != nil {
		fail(err)
	}
	start := time.Now()
	st := watch(m, func(s ddl.Status) bool { return s.Size > 0 && s.Have > s.Size*35/100 })
	m.Close()
	spec := m.Specs()[0]
	var saved int64
	for _, s := range spec.Segments {
		saved += s.Pos - s.Start
	}
	fmt.Printf("stopped at %d%% (%d segments, %d bytes recorded as on disk); resuming as after a restart\n",
		st.Have*100/st.Size, len(spec.Segments), saved)

	m2 := &ddl.Manager{Client: client, Connections: 4}
	if err := m2.Add(spec); err != nil {
		fail(err)
	}
	st = watch(m2, func(s ddl.Status) bool {
		return s.State == ddl.Done || s.State == ddl.Failed || s.State == ddl.NeedsLink
	})
	if st.State != ddl.Done {
		fail("ended", st.State, st.Error)
	}
	fmt.Printf("done in %s: %s\n", time.Since(start).Round(time.Second), st.Name)

	f, err := os.Open(filepath.Join(dir, st.Name))
	if err != nil {
		fail(err)
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		fail("SHA-256 MISMATCH:", got)
	}
	fmt.Println("SHA-256 matches")
}

// watch prints progress every 2 s until stop says so.
func watch(m *ddl.Manager, stop func(ddl.Status) bool) ddl.Status {
	for {
		time.Sleep(time.Second)
		s := m.Statuses()[0]
		if stop(s) {
			return s
		}
		if time.Now().Second()%2 == 0 && s.Size > 0 {
			fmt.Printf("  %s %5.1f%%  %6.1f MB/s\n", s.State, float64(s.Have)*100/float64(s.Size), float64(s.Rate)/1e6)
		}
	}
}

func fail(a ...any) {
	fmt.Println(a...)
	os.Exit(1)
}
