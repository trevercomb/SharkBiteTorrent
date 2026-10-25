package vpnprov

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Each test points a provider's API URL at a fake server that mimics the
// response formats captured from the real services.

func fake(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestMullvad(t *testing.T) {
	var mullvadPub string
	var mullvadDeleted bool
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wg":
			r.ParseForm()
			if r.Form.Get("account") != "1234567890123456" {
				w.WriteHeader(400)
				w.Write([]byte("Account does not exist"))
				return
			}
			if r.Form.Get("pubkey") == "" {
				t.Error("no pubkey sent")
			}
			mullvadPub = r.Form.Get("pubkey")
			w.Write([]byte("10.66.1.2/32,fc00:bbbb:bbbb:bb01::3:102/128\n"))
		case "/auth/v1/token":
			w.Write([]byte(`{"access_token":"tok","expiry":"2099-01-01T00:00:00Z"}`))
		case "/accounts/v1/devices":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`[{"id":"other","pubkey":"x"},{"id":"ours","pubkey":"` + mullvadPub + `"}]`))
		case "/accounts/v1/devices/ours":
			if r.Method == http.MethodDelete {
				mullvadDeleted = true
				w.WriteHeader(204)
			}
		case "/public/relays/wireguard/v1/":
			w.Write([]byte(`{"countries":[{"name":"Sweden","code":"se","cities":[{"name":"Malmo","relays":[{"hostname":"se-mma-wg-001","ipv4_addr_in":"193.138.218.220","public_key":"xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="}]}]},{"name":"USA","code":"us","cities":[{"name":"Dallas","relays":[{"hostname":"us-dal-wg-001","ipv4_addr_in":"198.54.1.1","public_key":"TrMvSoP4jYQlY6RIzBgbssQqY3vxI2Pi+y71lOWWXX0="}]}]}]}`))
		}
	})
	mullvadAPI = srv.URL
	p, _ := Get("mullvad")
	ctx := context.Background()

	if _, err := p.Login(ctx, map[string]string{"account": "999"}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("bad account: %v", err)
	}
	acc, err := p.Login(ctx, map[string]string{"account": "1234 5678 9012 3456"})
	if err != nil {
		t.Fatal(err)
	}
	if acc.Display != "Account ••••3456" {
		t.Errorf("display %q", acc.Display)
	}
	c, err := p.Connect(ctx, acc, "", "US")
	if err != nil {
		t.Fatal(err)
	}
	if c.Config.Endpoint != "198.54.1.1:51820" || len(c.Config.Addresses) != 2 || c.Config.DNS[0] != "10.64.0.1" {
		t.Errorf("config %+v", c.Config)
	}
	if err := p.Logout(ctx, acc); err != nil || !mullvadDeleted {
		t.Errorf("logout didn't remove our device: %v", err)
	}
	regions, _ := p.Regions(ctx)
	if len(regions) != 2 || regions[0].ID != "se" {
		t.Errorf("regions %+v", regions)
	}
}

func TestNordVPN(t *testing.T) {
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/users/services/credentials":
			if u, p, _ := r.BasicAuth(); u != "token" || p != "good" {
				w.WriteHeader(401)
				w.Write([]byte(`{"errors":{"message":"Unauthorized","code":101301}}`))
				return
			}
			w.Write([]byte(`{"nordlynx_private_key":"yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk="}`))
		case "/v1/servers/countries":
			w.Write([]byte(`[{"id":228,"name":"United States","code":"US"}]`))
		case "/v1/servers/recommendations":
			if r.URL.Query().Get("filters[country_id]") != "228" {
				t.Errorf("country filter %q", r.URL.RawQuery)
			}
			if r.URL.Query().Get("filters[servers_groups][identifier]") != "legacy_p2p" {
				t.Error("not filtering to P2P servers")
			}
			w.Write([]byte(`[{"hostname":"us14226.nordvpn.com","station":"187.14.116.80","locations":[{"country":{"name":"United States"}}],"technologies":[{"identifier":"wireguard_udp","metadata":[{"name":"public_key","value":"BnMo+8Ysw4GEQMx0P22PdpkrVPJQ1QhmX+Xna01SDEc="}]}]}]`))
		}
	})
	nordAPI = srv.URL
	p, _ := Get("nordvpn")
	ctx := context.Background()
	if _, err := p.Login(ctx, map[string]string{"token": "bad"}); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("bad token: %v", err)
	}
	acc, err := p.Login(ctx, map[string]string{"token": "good"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Connect(ctx, acc, "", "us")
	if err != nil {
		t.Fatal(err)
	}
	if c.Config.Endpoint != "187.14.116.80:51820" || c.Config.PeerPublicKey == "" || c.Server != "us14226 (United States)" {
		t.Errorf("connection %+v", c)
	}
}

func TestIVPN(t *testing.T) {
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v4/session/new":
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			switch {
			case req["username"] != "i-AAAA-BBBB-CCCC":
				w.Write([]byte(`{"status":401,"message":"The provided Account ID is not valid"}`))
			case req["confirmation"] == nil:
				w.Write([]byte(`{"status":70011,"message":"2FA required"}`))
			default:
				w.Write([]byte(`{"status":200,"token":"sess","wireguard":{"status":200,"ip_address":"172.21.5.9"}}`))
			}
		case "/v5/servers.json":
			w.Write([]byte(`{"wireguard":[{"gateway":"ar.wg.ivpn.net","country_code":"AR","country":"Argentina","city":"Buenos Aires","hosts":[{"hostname":"ar1.wg.ivpn.net","host":"149.22.83.100","public_key":"NXlWMTFlNJKQXfOnt+DJogxnDmfapEdgmHNat2JqwE0=","local_ip":"172.16.0.1/12"}]}],"config":{"ports":{"wireguard":[{"type":"UDP","port":53},{"type":"UDP","port":2049}]}}}`))
		case "/v4/session/delete":
			w.Write([]byte(`{"status":200}`))
		}
	})
	ivpnAPI = srv.URL
	p, _ := Get("ivpn")
	ctx := context.Background()

	if _, err := p.Login(ctx, map[string]string{"account": "nope"}); err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("bad account: %v", err)
	}
	_, err := p.Login(ctx, map[string]string{"account": "i-AAAA-BBBB-CCCC"})
	var need *NeedFieldError
	if !errors.As(err, &need) || need.Field.Key != "code2fa" {
		t.Fatalf("expected 2FA prompt, got %v", err)
	}
	acc, err := p.Login(ctx, map[string]string{"account": "i-AAAA-BBBB-CCCC", "code2fa": "123456"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Connect(ctx, acc, "AR", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Config.Endpoint != "149.22.83.100:2049" || c.Config.Addresses[0] != "172.21.5.9/32" || c.Config.DNS[0] != "172.16.0.1" {
		t.Errorf("config %+v", c.Config)
	}
	if err := p.Logout(ctx, acc); err != nil {
		t.Error(err)
	}
}

func TestPIALogin(t *testing.T) {
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		if r.FormValue("username") != "p1234567" || r.FormValue("password") != "pw" {
			w.WriteHeader(401)
			w.Write([]byte("HTTP Token: Access denied.\n"))
			return
		}
		w.Write([]byte(`{"token":"tok"}`))
	})
	piaTokenURL = srv.URL
	p, _ := Get("pia")
	ctx := context.Background()
	if _, err := p.Login(ctx, map[string]string{"username": "p1234567", "password": "bad"}); err == nil || !strings.Contains(err.Error(), "wrong username") {
		t.Fatalf("bad creds: %v", err)
	}
	acc, err := p.Login(ctx, map[string]string{"username": "p1234567", "password": "pw"})
	if err != nil || acc.Display != "p1234567" {
		t.Fatalf("%+v %v", acc, err)
	}
}

func TestPIACertificate(t *testing.T) {
	if cfg := piaTLS("x"); len(cfg.RootCAs.Subjects()) != 1 { //nolint:staticcheck
		t.Fatal("PIA CA certificate didn't load")
	}
}

func TestLiveServerLists(t *testing.T) {
	if testing.Short() {
		t.Skip("network")
	}
	// Public endpoints, no account needed: confirms the real formats still parse.
	mullvadAPI, nordAPI, ivpnAPI = "https://api.mullvad.net", "https://api.nordvpn.com", "https://api.ivpn.net"
	for _, id := range []string{"mullvad", "pia", "nordvpn", "ivpn"} {
		p, _ := Get(id)
		regions, err := p.Regions(context.Background())
		if err != nil || len(regions) < 10 {
			t.Errorf("%s: %d regions, %v", id, len(regions), err)
		}
	}
}

func TestErrorsHideTokens(t *testing.T) {
	// Nothing listens on port 1, so this fails with a *url.Error.
	req, _ := http.NewRequest("GET", "https://127.0.0.1:1/addKey?pt=SECRET-TOKEN&pubkey=abc", nil)
	_, err := do(req)
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") || strings.Contains(err.Error(), "addKey") {
		t.Fatalf("error leaks the URL: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error should still name the host: %v", err)
	}
	err = getJSON(context.Background(), apiClient(), "https://127.0.0.1:1/getSignature?token=SECRET-TOKEN", &struct{}{})
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("getJSON error leaks the URL: %v", err)
	}
}

func TestAPIUsesTunnelWhenSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer srv.Close()
	var viaTunnel int
	SetTunnel(func(ctx context.Context, network, addr string) (net.Conn, error) {
		viaTunnel++
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	})
	if err := getJSON(context.Background(), apiClient(), srv.URL, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := apiDial()(context.Background(), "tcp", srv.Listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if viaTunnel != 2 {
		t.Errorf("tunnel dials = %d, want 2", viaTunnel)
	}
	SetTunnel(nil)
	if err := getJSON(context.Background(), apiClient(), srv.URL, &struct{}{}); err != nil || viaTunnel != 2 {
		t.Errorf("after SetTunnel(nil): err=%v, tunnel dials=%d", err, viaTunnel)
	}
}

// A saved PIA token is reused until it's due for renewal, instead of
// signing in on every connect (which gets the account rate-limited).
func TestPIATokenReuse(t *testing.T) {
	logins := 0
	tokens := fake(t, func(w http.ResponseWriter, r *http.Request) {
		logins++
		w.Write([]byte(`{"token":"fresh"}`))
	})
	list := fake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	keepPIAURLs(t)
	piaTokenURL, piaServerList = tokens.URL, list.URL
	p, _ := Get("pia")
	ctx := context.Background()

	acc, err := p.Login(ctx, map[string]string{"username": "p1234567", "password": "pw"})
	if err != nil || savedToken(acc, time.Now()) != "fresh" {
		t.Fatalf("login didn't save its token: %+v %v", acc, err)
	}
	logins = 0
	p.Connect(ctx, acc, "", "US") // fails at the server list; only the sign-in matters here
	if logins != 0 {
		t.Errorf("signed in again with a saved token (%d)", logins)
	}

	acc.Data[piaToken] = "old"
	acc.Data[piaTokenExpires] = strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	p.Connect(ctx, acc, "", "US")
	if logins != 1 || savedToken(acc, time.Now()) != "fresh" {
		t.Errorf("expired token: %d sign-ins, token %q", logins, acc.Data[piaToken])
	}
	if savedToken(acc, time.Now().Add(piaTokenUse+time.Minute)) != "" {
		t.Error("a token is used past piaTokenUse")
	}
}

func TestRateLimit(t *testing.T) {
	srv := fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	keepPIAURLs(t)
	piaTokenURL = srv.URL
	p, _ := Get("pia")
	_, err := p.Login(context.Background(), map[string]string{"username": "p1234567", "password": "pw"})
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 2*time.Minute {
		t.Fatalf("sign-in: %v", err)
	}
	err = getJSON(context.Background(), srv.Client(), srv.URL, &struct{}{})
	if !errors.As(err, &rl) {
		t.Fatalf("getJSON: %v", err)
	}

	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	if d := retryAfter(now.Add(5*time.Minute).Format(http.TimeFormat), now); d != 5*time.Minute {
		t.Errorf("HTTP date: %v", d)
	}
	if d := retryAfter("", now); d != 0 {
		t.Errorf("none: %v", d)
	}
}

// keepPIAURLs puts PIA's real API URLs back after the test.
func keepPIAURLs(t *testing.T) {
	tok, list := piaTokenURL, piaServerList
	t.Cleanup(func() { piaTokenURL, piaServerList = tok, list })
}
