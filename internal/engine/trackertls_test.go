package engine

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A client configured like the library's tracker client: certificate checks
// off, dialing through our hook.
func libraryClient() *http.Client {
	var d net.Dialer
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext:     verifiedTrackerDial(d.DialContext),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "example.com"},
	}}
}

func TestTrackerTLSVerified(t *testing.T) {
	ok := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "announce ok")
	}))
	defer ok.Close()
	pool := x509.NewCertPool()
	pool.AddCert(ok.Certificate())
	trackerRoots = pool
	t.Cleanup(func() { trackerRoots = nil })

	// A trusted tracker works through the bridge.
	resp, err := libraryClient().Get(ok.URL + "/announce?passkey=x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "announce ok" {
		t.Fatalf("got %q", body)
	}

	// One whose certificate isn't trusted never receives the request.
	var reached bool
	bad := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	// The client is meant to reject this certificate; don't log the handshake.
	bad.Config.ErrorLog = log.New(io.Discard, "", 0)
	other, err := bridgeCert() // any certificate the pool doesn't trust
	if err != nil {
		t.Fatal(err)
	}
	bad.TLS = &tls.Config{Certificates: []tls.Certificate{other}}
	bad.StartTLS()
	defer bad.Close()
	if _, err := libraryClient().Get(bad.URL + "/announce?passkey=x"); err == nil || reached {
		t.Fatalf("untrusted tracker: err %v, reached %v", err, reached)
	}

	// Plain HTTP passes straight through.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "plain") }))
	defer plain.Close()
	resp, err = libraryClient().Get(plain.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "plain" {
		t.Fatalf("plain: %q", body)
	}
}
