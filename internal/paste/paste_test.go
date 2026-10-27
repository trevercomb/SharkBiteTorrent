package paste

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestLinks(t *testing.T) {
	text := `Mirrors:
https://host.example/f/abc123#game.part01.rar
(https://cdn.example/files/Big%20File.part02.rar).
<a href="https://cdn.example/x.torrent?a=1&amp;b=2">t</a>
magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x
magnet:?dn=no-hash
https://host.example/f/abc123#game.part01.rar
https://dl.example/archive.7z.001, https://en.wikipedia.org/wiki/Foo_(bar)`
	got := Links(text)
	want := []Link{
		{URL: "https://host.example/f/abc123#game.part01.rar", Host: "host.example", Kind: Page},
		{URL: "https://cdn.example/files/Big%20File.part02.rar", Host: "cdn.example", Name: "Big File.part02.rar", Kind: File},
		{URL: "https://cdn.example/x.torrent?a=1&b=2", Host: "cdn.example", Name: "x.torrent", Kind: Torrent},
		{URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=x", Kind: Magnet},
		{URL: "https://dl.example/archive.7z.001", Host: "dl.example", Name: "archive.7z.001", Kind: File},
		{URL: "https://en.wikipedia.org/wiki/Foo_(bar)", Host: "en.wikipedia.org", Kind: Page},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d links, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("link %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Folder listings and archive pages link relatively; those links are found
// and resolved against the page, without its sort and parent links.
func TestLinksFromPage(t *testing.T) {
	listing := `<html><body><h1>Index of /releases/x86_64</h1>
<a href="?C=N;O=D">Name</a> <a href="?C=M;O=A">Last modified</a>
<a href="/releases/">Parent Directory</a>
<a href="alpine-standard-3.20.3-x86_64.iso">alpine-standard-3.20.3-x86_64.iso</a>
<a href='alpine-standard-3.20.3-x86_64.iso.sha256'>sha256</a>
<a href="netboot/">netboot/</a>
<a href="#top">top</a> <a href="mailto:x@example.org">mail</a> <a href="javascript:void(0)">js</a>
<a href="https://mirror.example.org/other.iso">elsewhere</a>
</body></html>`
	base, _ := url.Parse("https://dl.example.org/releases/x86_64/")
	got := LinksFrom(listing, base)
	want := []string{
		"https://dl.example.org/releases/x86_64/alpine-standard-3.20.3-x86_64.iso",
		"https://dl.example.org/releases/x86_64/alpine-standard-3.20.3-x86_64.iso.sha256",
		"https://dl.example.org/releases/x86_64/netboot/",
		"https://mirror.example.org/other.iso",
	}
	var urls []string
	for _, l := range got {
		urls = append(urls, l.URL)
	}
	if strings.Join(urls, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(urls, "\n"), strings.Join(want, "\n"))
	}
	if got[0].Kind != File || got[0].Name != "alpine-standard-3.20.3-x86_64.iso" {
		t.Errorf("first: %+v", got[0])
	}

	// An archive's download page: root-relative and protocol-relative links.
	page := `<a href="/download/some-item/Some%20Book.epub">EPUB</a> <a href="//archive.example.org/download/some-item/scan.pdf">PDF</a>`
	base, _ = url.Parse("https://archive.example.org/details/some-item")
	got = LinksFrom(page, base)
	if len(got) != 2 || got[0].URL != "https://archive.example.org/download/some-item/Some%20Book.epub" || got[0].Name != "Some Book.epub" ||
		got[1].URL != "https://archive.example.org/download/some-item/scan.pdf" {
		t.Errorf("archive page: %+v", got)
	}

	// Without a page (pasted text), relative links mean nothing: not found.
	if got := Links(listing); len(got) != 1 {
		t.Errorf("pasted text: %+v", got)
	}
}

// Find reads a page and resolves its relative links against where it ended
// up after redirects.
func TestFindRelative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/files/", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<a href="a.zip">a</a> <a href="b.iso">b</a>`))
	}))
	defer srv.Close()
	got, err := Find(context.Background(), srv.Client(), srv.URL+"/old")
	if err != nil || len(got) != 2 || got[0].URL != srv.URL+"/files/a.zip" || got[1].URL != srv.URL+"/files/b.iso" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestBase58(t *testing.T) {
	// "1" is a zero digit: the key is padded to 32 bytes either way.
	for _, s := range []string{"1", "2", "GJdz7a5tnug32Uw4FB8uydUoXtjCCxaeRPvCewfYCFPq"} {
		b, err := base58Decode(s)
		if err != nil || len(b) != 32 {
			t.Errorf("%q: %d bytes, %v", s, len(b), err)
		}
	}
	if b, _ := base58Decode("2"); b[31] != 1 {
		t.Errorf("2 decodes to %x", b)
	}
	for _, s := range []string{"", "0OIl", strings.Repeat("z", 50)} {
		if _, err := base58Decode(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func base58Encode(b []byte) string {
	n := new(big.Int).SetBytes(b)
	var out []byte
	for n.Sign() > 0 {
		m := new(big.Int)
		n.DivMod(n, big.NewInt(58), m)
		out = append([]byte{base58Alphabet[m.Int64()]}, out...)
	}
	return string(out)
}

// encryptPrivateBin builds a v2 paste the way PrivateBin's browser code does.
// The server's JSON escapes "/" as PHP does, which decryption must undo.
func encryptPrivateBin(t *testing.T, key []byte, text string, zlib bool) []byte {
	t.Helper()
	iv := bytes.Repeat([]byte{7}, 16)
	salt := bytes.Repeat([]byte{9}, 8)
	comp := "none"
	if zlib {
		comp = "zlib"
	}
	adata := []any{[]any{base64.StdEncoding.EncodeToString(iv), base64.StdEncoding.EncodeToString(salt), 1000, 256, 128, "aes", "gcm", comp}, "plaintext", 0, 0}
	var ad bytes.Buffer
	enc := json.NewEncoder(&ad)
	enc.SetEscapeHTML(false)
	enc.Encode(adata)
	adJS := bytes.TrimSpace(ad.Bytes())

	plain, _ := json.Marshal(map[string]string{"paste": text})
	if zlib {
		var z bytes.Buffer
		w, _ := flate.NewWriter(&z, flate.BestCompression)
		w.Write(plain)
		w.Close()
		plain = z.Bytes()
	}
	k, _ := pbkdf2.Key(sha256.New, string(key), salt, 1000, 32)
	block, _ := aes.NewCipher(k)
	gcm, _ := cipher.NewGCMWithNonceSize(block, 16)
	ct := gcm.Seal(nil, iv, plain, adJS)
	php := strings.ReplaceAll(string(adJS), "/", `\/`)
	return []byte(fmt.Sprintf(`{"status":0,"id":"52ea850ae04981ff","v":2,"adata":%s,"ct":"%s","meta":{}}`,
		php, strings.ReplaceAll(base64.StdEncoding.EncodeToString(ct), "/", `\/`)))
}

func TestReadPrivateBin(t *testing.T) {
	key := bytes.Repeat([]byte{0xab}, 32)
	key[0] = 0 // leading zero byte: base58 drops it, decoding must pad it back
	const text = "part 1: https://host.example/f/1\npart 2: https://host.example/f/2"
	for _, zlib := range []bool{false, true} {
		body := encryptPrivateBin(t, key, text, zlib)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Requested-With") != "JSONHttpRequest" {
				http.Error(w, "page", http.StatusBadRequest)
				return
			}
			w.Write(body)
		}))
		addr := srv.URL + "/?52ea850ae04981ff#" + base58Encode(key)
		if u, _ := url.Parse(addr); !looksLikePrivateBin(u) {
			t.Fatalf("%s not recognised", addr)
		}
		got, err := Read(context.Background(), srv.Client(), addr)
		if err != nil || got != text {
			t.Errorf("zlib=%v: got %q, %v", zlib, got, err)
		}
		wrong := bytes.Repeat([]byte{0xcd}, 32)
		if _, err := Read(context.Background(), srv.Client(), srv.URL+"/?52ea850ae04981ff#"+base58Encode(wrong)); !errors.Is(err, ErrPassword) {
			t.Errorf("wrong key: %v", err)
		}
		srv.Close()
	}
}

func TestReadText(t *testing.T) {
	got, err := Read(context.Background(), nil, "  https://a.example/x and https://b.example/y ")
	if err != nil || got != "https://a.example/x and https://b.example/y" {
		t.Errorf("got %q, %v", got, err)
	}
	// A single link to a file is itself the list: nothing is fetched.
	const iso = "https://dl.example/releases/os-1.0.iso"
	if got, err := Read(context.Background(), nil, iso); err != nil || got != iso {
		t.Errorf("file link: got %q, %v", got, err)
	}
	// So is an address that turns out to be a file.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("binary"))
	}))
	defer srv.Close()
	if got, err := Read(context.Background(), srv.Client(), srv.URL+"/get?id=5"); err != nil || got != srv.URL+"/get?id=5" {
		t.Errorf("file behind a plain address: got %q, %v", got, err)
	}
	// Plain text without links, or anything sent as an attachment, is a
	// file too; plain text with links is the list.
	txt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readme.txt":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("just some text"))
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Disposition", `attachment; filename="page.html"`)
			w.Write([]byte(`<a href="https://x.example/a.zip">a</a>`))
		default:
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("https://x.example/a.zip\nhttps://x.example/b.zip"))
		}
	}))
	defer txt.Close()
	for _, p := range []string{"/readme.txt", "/page"} {
		if got, err := Read(context.Background(), txt.Client(), txt.URL+p); err != nil || got != txt.URL+p {
			t.Errorf("%s: got %q, %v", p, got, err)
		}
	}
	if got, err := Find(context.Background(), txt.Client(), txt.URL+"/list.txt"); err != nil || len(got) != 2 {
		t.Errorf("link list: got %+v, %v", got, err)
	}
}

// A page that answers 404 (file hosts that show pages to browsers only) is
// reported as an HTTPError, so the app can open it in its browser window.
func TestFindHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer srv.Close()
	_, err := Find(t.Context(), srv.Client(), srv.URL+"/abc123")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 404 {
		t.Fatalf("err %v, want an HTTPError 404", err)
	}
	if l, ok := PageLink(srv.URL + "/abc123"); !ok || l.Kind != Page || l.Host != "127.0.0.1" {
		t.Errorf("PageLink: %+v %v", l, ok)
	}
}
