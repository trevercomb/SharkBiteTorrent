package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sharkbite/internal/engine"
	"strconv"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"sharkbite/internal/search"
	"sharkbite/internal/secret"
	"sharkbite/internal/socks"
)

// A local SOCKS5 proxy that leads into the VPN tunnel, so the user's
// Prowlarr or Jackett can reach indexer sites through the VPN too. It only
// listens on 127.0.0.1 and needs a random password. While the app is closed
// or the VPN is down, connections through it fail rather than going out
// directly. Settings are stored encrypted in proxy.json.

type proxyConfig struct {
	Enabled  bool   `json:"enabled"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type ProxyStatus struct {
	Enabled bool `json:"enabled"`
	Running bool `json:"running"`
	// Applied: the server (Prowlarr) is currently set to use this proxy.
	// Manual: the server (Jackett) has to be pointed at it by hand.
	Applied  bool   `json:"applied"`
	Manual   bool   `json:"manual"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Error    string `json:"error"`
}

const proxyHost = "127.0.0.1"

func (a *App) proxyPath() string { return filepath.Join(a.configDir, "proxy.json") }

func (a *App) loadProxyConfig() proxyConfig {
	var c proxyConfig
	if data, err := secret.ReadFile(a.proxyPath()); err == nil {
		json.Unmarshal(data, &c)
	}
	return c
}

func (a *App) saveProxyConfig(c proxyConfig) error {
	data, _ := json.Marshal(c)
	return secret.WriteFile(a.proxyPath(), data)
}

func (c proxyConfig) search() search.Proxy {
	return search.Proxy{Host: proxyHost, Port: c.Port, Username: c.Username, Password: c.Password}
}

// dialTunnel is the proxy's way out: only ever through the VPN. With "No
// VPN" chosen it refuses too, so a proxied Prowlarr never quietly switches
// to the real IP; Prowlarr without the proxy keeps working on its own.
func (a *App) dialTunnel(ctx context.Context, network, addr string) (net.Conn, error) {
	a.mu.Lock()
	tun := a.tun
	a.mu.Unlock()
	if tun == nil {
		return nil, errors.New("VPN not connected")
	}
	return tun.DialContext(ctx, network, addr)
}

// directMode reports whether the user chose "No VPN" and the engine runs on
// the normal network.
func (a *App) directMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.vpn.Provider == "none" && a.vpn.State == "off"
}

// searchDial is how search downloads reach hosts other than the user's
// Prowlarr/Jackett: through the VPN, or directly when "No VPN" is chosen.
func (a *App) searchDial(ctx context.Context, network, addr string) (net.Conn, error) {
	if a.directMode() {
		return a.direct.dial(ctx, network, addr, false)
	}
	return a.dialTunnel(ctx, network, addr)
}

// startProxy runs the proxy if it's turned on. It keeps running across VPN
// reconnects; dialTunnel always uses the current tunnel.
func (a *App) startProxy() {
	c := a.loadProxyConfig()
	if !c.Enabled {
		return
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(proxyHost, strconv.Itoa(c.Port)))
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.proxyErr = "couldn't start the proxy on port " + strconv.Itoa(c.Port) + ": " + err.Error()
		return
	}
	s := &socks.Server{Username: c.Username, Password: c.Password, Dial: a.dialTunnel}
	a.proxy, a.proxyErr = s, ""
	go func() {
		if err := s.Serve(ln); err != nil {
			runtime.LogWarningf(a.ctx, "%s", engine.Redact(fmt.Sprintf("proxy: %v", err)))
		}
	}()
}

func (a *App) stopProxy() {
	a.mu.Lock()
	s := a.proxy
	a.proxy = nil
	a.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

// freePort picks a port that's free right now; it's then kept, so the
// server's settings stay valid across restarts.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", proxyHost+":0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func randomToken() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// --- Bound to the frontend ---

func (a *App) GetProxyStatus() ProxyStatus {
	c := a.loadProxyConfig()
	a.mu.Lock()
	st := ProxyStatus{Enabled: c.Enabled, Running: a.proxy != nil, Error: a.proxyErr}
	a.mu.Unlock()
	if !c.Enabled {
		return st
	}
	st.Host, st.Port, st.Username, st.Password = proxyHost, c.Port, c.Username, c.Password
	if a.directMode() && st.Error == "" {
		st.Error = "The VPN is turned off (No VPN), so searches through this proxy fail instead of using your real IP. " +
			"Turn this option off to let it search over your normal connection."
	}
	if sc, ok := a.searchConfig(); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		on, err := search.ProxyEnabled(ctx, sc, c.search())
		switch {
		case errors.Is(err, search.ErrManualProxy):
			st.Manual = true
		case err != nil:
			if st.Error == "" {
				st.Error = err.Error()
			}
		default:
			st.Applied = on
		}
	}
	return st
}

// SetSearchProxy turns routing Prowlarr/Jackett through the VPN on or off.
// For Prowlarr it changes Prowlarr's own proxy setting too; Jackett has to be
// set by hand with the values from GetProxyStatus.
func (a *App) SetSearchProxy(enable bool) (ProxyStatus, error) {
	c := a.loadProxyConfig()
	sc, haveServer := a.searchConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if !enable {
		if haveServer && c.Port != 0 {
			if err := search.SetProxy(ctx, sc, c.search(), false); err != nil && !errors.Is(err, search.ErrManualProxy) {
				return a.GetProxyStatus(), err
			}
		}
		a.stopProxy()
		c.Enabled = false
		if err := a.saveProxyConfig(c); err != nil {
			return a.GetProxyStatus(), err
		}
		return a.GetProxyStatus(), nil
	}

	if !haveServer {
		return ProxyStatus{}, errors.New("connect Prowlarr or Jackett first")
	}
	if c.Port == 0 {
		p, err := freePort()
		if err != nil {
			return ProxyStatus{}, err
		}
		c.Port = p
	}
	if c.Username == "" || c.Password == "" {
		c.Username, c.Password = "sharkbite", randomToken()
	}
	c.Enabled = true
	if err := a.saveProxyConfig(c); err != nil {
		return ProxyStatus{}, err
	}
	a.stopProxy()
	a.startProxy()
	if err := search.SetProxy(ctx, sc, c.search(), true); err != nil && !errors.Is(err, search.ErrManualProxy) {
		return a.GetProxyStatus(), err
	}
	return a.GetProxyStatus(), nil
}

// disableSearchProxy is used when the search server is disconnected: the
// proxy settings belong to that server.
func (a *App) disableSearchProxy() {
	if a.loadProxyConfig().Enabled {
		a.SetSearchProxy(false)
	}
	if err := os.Remove(a.proxyPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		runtime.LogWarningf(a.ctx, "%s", engine.Redact(fmt.Sprintf("proxy: %v", err)))
	}
}

// ProxyCheck is the result of CheckSearchProxy.
type ProxyCheck struct {
	Used        bool  `json:"used"`        // the server's traffic went through the proxy
	Connections int64 `json:"connections"` // connections it made through it during the test
}

// CheckSearchProxy confirms the search server really uses the proxy (for
// Jackett, whose proxy the user sets by hand). It runs a search for a random
// word, so the server can't answer from its cache and has to contact its
// sites, and counts the connections that came through the proxy meanwhile.
func (a *App) CheckSearchProxy() (ProxyCheck, error) {
	sc, ok := a.searchConfig()
	if !ok {
		return ProxyCheck{}, errors.New("connect Prowlarr or Jackett first")
	}
	a.mu.Lock()
	s := a.proxy
	a.mu.Unlock()
	if s == nil {
		return ProxyCheck{}, errors.New("the proxy isn't running; turn the option on first")
	}
	before, sentBefore := s.Requests.Load(), s.Sent.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := search.Search(ctx, sc, "sharkbite proxy check "+randomToken()[:8], ""); err != nil {
		return ProxyCheck{}, err
	}
	time.Sleep(time.Second) // let connections still being opened arrive
	n := s.Requests.Load() - before
	// A search right after another goes over the connections that one left
	// open: no new connection, but its requests still pass through here.
	return ProxyCheck{Used: n > 0 || s.Sent.Load() > sentBefore, Connections: n}, nil
}
