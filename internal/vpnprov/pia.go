package vpnprov

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"sharkbite/internal/tunnel"
)

// Private Internet Access: username/password → token, then each connection
// registers a fresh WireGuard key with the chosen server. Mirrors PIA's
// official scripts at github.com/pia-foss/manual-connections, including port
// forwarding (getSignature/bindPort through the tunnel).
type pia struct{}

var (
	piaTokenURL   = "https://www.privateinternetaccess.com/api/client/v2/token"
	piaServerList = "https://serverlist.piaservers.net/vpninfo/servers/v6"
)

//go:embed pia_ca.crt
var piaCA []byte

func init() { register(pia{}) }

func (pia) Info() Info {
	return Info{
		ID:   "pia",
		Name: "Private Internet Access",
		Fields: []Field{
			{Key: "username", Label: "Username", Kind: "text", Help: "Starts with p, e.g. p1234567"},
			{Key: "password", Label: "Password", Kind: "password"},
		},
		PortForwarding: true,
		Note:           "Pick a location marked “port forwarding” so other peers can connect to you (not available in the US).",
	}
}

func (pia) token(ctx context.Context, user, pass string) (string, error) {
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	mw.WriteField("username", user)
	mw.WriteField("password", pass)
	mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, piaTokenURL, strings.NewReader(body.String()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := do(req)
	if err != nil {
		return "", fmt.Errorf("couldn't reach PIA: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errors.New("PIA: wrong username or password")
	}
	var r struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.Token == "" {
		return "", fmt.Errorf("PIA login failed (HTTP %d)", resp.StatusCode)
	}
	return r.Token, nil
}

func (p pia) Login(ctx context.Context, creds map[string]string) (*Account, error) {
	user, pass := strings.TrimSpace(creds["username"]), creds["password"]
	if user == "" || pass == "" {
		return nil, errors.New("enter your PIA username and password")
	}
	token, err := p.token(ctx, user, pass)
	if err != nil {
		return nil, err
	}
	// Tokens expire after a day, so keep the password (encrypted at rest by
	// the app) to get a new one when the saved token is too old.
	acc := &Account{Provider: "pia", Display: user, Data: map[string]string{"username": user, "password": pass}}
	saveToken(acc, token, time.Now())
	return acc, nil
}

// PIA's tokens last 24 hours. One is reused for piaTokenUse, so it never
// runs out during a connect: signing in for every connect (each start,
// update and Retry) gets an account rate-limited (HTTP 429) quickly.
const (
	piaTokenUse     = 20 * time.Hour
	piaToken        = CachePrefix + "token"
	piaTokenExpires = CachePrefix + "tokenExpires" // unix seconds
)

func saveToken(acc *Account, token string, now time.Time) {
	acc.Data[piaToken] = token
	acc.Data[piaTokenExpires] = strconv.FormatInt(now.Add(piaTokenUse).Unix(), 10)
}

// savedToken returns the account's token if it's still good to use.
func savedToken(acc *Account, now time.Time) string {
	exp, err := strconv.ParseInt(acc.Data[piaTokenExpires], 10, 64)
	if err != nil || !now.Before(time.Unix(exp, 0)) {
		return ""
	}
	return acc.Data[piaToken]
}

// freshToken signs in for a new token and saves it in acc.
func (p pia) freshToken(ctx context.Context, acc *Account) (string, error) {
	token, err := p.token(ctx, acc.Data["username"], acc.Data["password"])
	if err != nil {
		return "", err
	}
	saveToken(acc, token, time.Now())
	return token, nil
}

func (pia) Logout(context.Context, *Account) error { return nil }

type piaRegion struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Country     string `json:"country"`
	PortForward bool   `json:"port_forward"`
	Offline     bool   `json:"offline"`
	Servers     struct {
		WG []struct {
			IP string `json:"ip"`
			CN string `json:"cn"`
		} `json:"wg"`
	} `json:"servers"`
}

func (pia) regions(ctx context.Context) ([]piaRegion, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, piaServerList, nil)
	if err != nil {
		return nil, err
	}
	resp, err := do(req)
	if err != nil {
		return nil, fmt.Errorf("PIA server list: %w", err)
	}
	defer resp.Body.Close()
	// The body is one line of JSON followed by a signature.
	var r struct {
		Regions []piaRegion `json:"regions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("PIA server list: %w", err)
	}
	var out []piaRegion
	for _, reg := range r.Regions {
		if !reg.Offline && len(reg.Servers.WG) > 0 {
			out = append(out, reg)
		}
	}
	return out, nil
}

func (p pia) Regions(ctx context.Context) ([]Region, error) {
	regs, err := p.regions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Region, len(regs))
	for i, r := range regs {
		out[i] = Region{ID: r.ID, Name: r.Name, PortForward: r.PortForward}
	}
	return out, nil
}

// piaTLS returns a TLS config trusting only PIA's CA, for talking to a VPN
// server by IP while verifying its certificate name (cn).
func piaTLS(cn string) *tls.Config {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(piaCA)
	return &tls.Config{RootCAs: pool, ServerName: cn}
}

// piaClient makes requests to https://<cn>:<port>/ that actually connect to
// ip, through dial (direct or via the tunnel).
func piaClient(ip, cn string, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   piaTLS(cn),
			DisableKeepAlives: true, // addKey goes direct (before the tunnel); don't leave it open
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, port, _ := net.SplitHostPort(addr)
				return dial(ctx, network, net.JoinHostPort(ip, port))
			},
		},
	}
}

// Connect may save a new token in acc.Data; the caller saves the account.
func (p pia) Connect(ctx context.Context, acc *Account, region, home string) (*Connection, error) {
	var err error
	token := savedToken(acc, time.Now())
	saved := token != ""
	if !saved {
		if token, err = p.freshToken(ctx, acc); err != nil {
			return nil, err
		}
	}
	regs, err := p.regions(ctx)
	if err != nil {
		return nil, err
	}
	var chosen *piaRegion
	for i := range regs {
		if regs[i].ID == region {
			chosen = &regs[i]
		}
	}
	if chosen == nil { // automatic: same country, preferring port forwarding
		for i := range regs {
			if strings.EqualFold(regs[i].Country, home) && (chosen == nil || regs[i].PortForward && !chosen.PortForward) {
				chosen = &regs[i]
			}
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("PIA has no servers in your country; pick a location in Settings")
	}
	srv := pick(chosen.Servers.WG)

	priv, pub, err := newKeypair()
	if err != nil {
		return nil, err
	}
	// addKey registers our key with the chosen server: through the current
	// tunnel when switching from another VPN, otherwise directly.
	hc := piaClient(srv.IP, srv.CN, apiDial())
	var wg struct {
		Status     string   `json:"status"`
		Message    string   `json:"message"`
		ServerKey  string   `json:"server_key"`
		ServerPort int      `json:"server_port"`
		PeerIP     string   `json:"peer_ip"`
		DNS        []string `json:"dns_servers"`
	}
	// addKey's refused reports whether the server answered with a refusal
	// (as opposed to not being reachable).
	addKey := func(token string) (refused bool, err error) {
		q := url.Values{"pt": {token}, "pubkey": {pub}}
		if err := getJSON(ctx, hc, "https://"+srv.CN+":1337/addKey?"+q.Encode(), &wg); err != nil {
			s := statusOf(err)
			return s/100 == 4 && s != http.StatusTooManyRequests, fmt.Errorf("PIA addKey: %w", err)
		}
		if wg.Status != "OK" {
			return true, fmt.Errorf("PIA addKey: %s %s", wg.Status, wg.Message)
		}
		return false, nil
	}
	refused, err := addKey(token)
	if refused && saved {
		// The saved token isn't accepted any more (the password was
		// changed, say): sign in once for a new one and try again.
		if token, err = p.freshToken(ctx, acc); err != nil {
			return nil, err
		}
		_, err = addKey(token)
	}
	if err != nil {
		return nil, err
	}

	conn := &Connection{
		Server: chosen.Name + " (" + srv.CN + ")",
		Config: tunnel.Config{
			PrivateKey:    priv,
			PeerPublicKey: wg.ServerKey,
			Endpoint:      net.JoinHostPort(srv.IP, strconv.Itoa(wg.ServerPort)),
			Addresses:     []string{wg.PeerIP},
			DNS:           wg.DNS,
		},
	}
	if chosen.PortForward {
		conn.PortForward = func(ctx context.Context, t *tunnel.Tunnel) (int, func(context.Context) error, time.Duration, error) {
			return piaPortForward(ctx, t, srv.IP, srv.CN, token)
		}
	}
	return conn, nil
}

// piaPortForward asks the server for a port (getSignature) and binds it.
// bindPort must be repeated every 15 minutes or the port is released.
func piaPortForward(ctx context.Context, t *tunnel.Tunnel, ip, cn, token string) (int, func(context.Context) error, time.Duration, error) {
	hc := piaClient(ip, cn, t.DialContext)
	base := "https://" + cn + ":19999"
	var sig struct {
		Status    string `json:"status"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}
	if err := getJSON(ctx, hc, base+"/getSignature?"+url.Values{"token": {token}}.Encode(), &sig); err != nil {
		return 0, nil, 0, fmt.Errorf("PIA port forwarding: %w", err)
	}
	if sig.Status != "OK" {
		return 0, nil, 0, fmt.Errorf("PIA port forwarding: %s", sig.Status)
	}
	raw, err := base64.StdEncoding.DecodeString(sig.Payload)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("PIA port forwarding payload: %w", err)
	}
	var payload struct {
		Port      int    `json:"port"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Port == 0 {
		return 0, nil, 0, fmt.Errorf("PIA port forwarding payload: %v", err)
	}
	expires, _ := time.Parse(time.RFC3339Nano, payload.ExpiresAt)
	bind := func(ctx context.Context) error {
		// The signed payload is good for about two months; renew well before.
		if !expires.IsZero() && time.Until(expires) < 24*time.Hour {
			return ErrRenew
		}
		var r struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}
		q := url.Values{"payload": {sig.Payload}, "signature": {sig.Signature}}
		if err := getJSON(ctx, hc, base+"/bindPort?"+q.Encode(), &r); err != nil {
			return err
		}
		if r.Status != "OK" {
			return fmt.Errorf("bindPort: %s %s", r.Status, r.Message)
		}
		return nil
	}
	if err := bind(ctx); err != nil {
		return 0, nil, 0, fmt.Errorf("PIA port forwarding: %w", err)
	}
	return payload.Port, bind, 15 * time.Minute, nil
}
