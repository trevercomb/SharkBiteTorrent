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
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
)

// PrivateBin pastes are encrypted in the browser: the server only has the
// ciphertext, and the key is the URL's fragment (after #), which browsers
// never send. We do the same: fetch the JSON, decrypt here.

// ErrPassword is returned for a paste that also needs a password.
var ErrPassword = errors.New("this paste is protected with a password")

// looksLikePrivateBin is true for the PrivateBin URL shape: "?<16 hex>" and
// a base58 key in the fragment.
func looksLikePrivateBin(u *url.URL) bool {
	id := strings.TrimSuffix(u.RawQuery, "=")
	if len(id) != 16 || u.Fragment == "" {
		return false
	}
	for _, c := range id {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	key := strings.TrimPrefix(u.Fragment, "-") // "#-" marks "load confirmation" pastes
	_, err := base58Decode(key)
	return err == nil
}

type pbPaste struct {
	Status  int             `json:"status"`
	Message string          `json:"message"`
	V       int             `json:"v"`
	AData   json.RawMessage `json:"adata"`
	CT      string          `json:"ct"`
}

func readPrivateBin(ctx context.Context, hc *http.Client, u *url.URL) (string, error) {
	key, err := base58Decode(strings.TrimPrefix(u.Fragment, "-"))
	if err != nil {
		return "", fmt.Errorf("paste key: %w", err)
	}
	api := *u
	api.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Requested-With", "JSONHttpRequest") // asks PrivateBin for JSON instead of the page
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("paste: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	if err != nil {
		return "", err
	}
	var p pbPaste
	if err := json.Unmarshal(body, &p); err != nil {
		return "", fmt.Errorf("paste: not a PrivateBin answer: %w", err)
	}
	if p.Status != 0 {
		if p.Message == "" {
			p.Message = "the paste couldn't be read"
		}
		return "", fmt.Errorf("paste: %s", p.Message)
	}
	return decryptPrivateBin(p, key)
}

// decryptPrivateBin handles format v2 (PrivateBin 1.3 and later, 2019): AES-256-GCM
// with a PBKDF2-SHA256 key and the JSON of adata as additional data.
func decryptPrivateBin(p pbPaste, key []byte) (string, error) {
	if p.V != 2 {
		return "", fmt.Errorf("paste: PrivateBin format %d isn't supported (only 2)", p.V)
	}
	// adata is [[iv, salt, iterations, keybits, tagbits, "aes", "gcm", compression], formatter, discussion, burn]
	// or, for comments, just the inner list.
	var outer []json.RawMessage
	if err := json.Unmarshal(p.AData, &outer); err != nil || len(outer) == 0 {
		return "", errors.New("paste: bad adata")
	}
	spec := p.AData
	if len(outer[0]) > 0 && outer[0][0] == '[' {
		spec = outer[0]
	}
	var f []any
	dec := json.NewDecoder(bytes.NewReader(spec))
	dec.UseNumber()
	if err := dec.Decode(&f); err != nil || len(f) < 8 {
		return "", errors.New("paste: bad cipher parameters")
	}
	str := func(i int) string { s, _ := f[i].(string); return s }
	num := func(i int) int {
		n, _ := f[i].(json.Number)
		v, _ := n.Int64()
		return int(v)
	}
	iv, err1 := base64.StdEncoding.DecodeString(str(0))
	salt, err2 := base64.StdEncoding.DecodeString(str(1))
	iter, keyBits, tagBits := num(2), num(3), num(4)
	if err1 != nil || err2 != nil || str(5) != "aes" || str(6) != "gcm" ||
		keyBits != 256 || tagBits != 128 || iter < 1 || iter > 10_000_000 || len(iv) == 0 {
		return "", errors.New("paste: unsupported cipher parameters")
	}
	ct, err := base64.StdEncoding.DecodeString(p.CT)
	if err != nil {
		return "", errors.New("paste: bad ciphertext")
	}
	// The browser hashes the key exactly as decoded; a password would be
	// appended to it, which we don't support (yet).
	k, err := pbkdf2.Key(sha256.New, string(key), salt, iter, keyBits/8)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, iv, ct, jsAData(p.AData))
	if err != nil {
		// Wrong key, or a password is needed: PrivateBin can't tell either.
		return "", fmt.Errorf("paste: couldn't decrypt (wrong link, or %w)", ErrPassword)
	}
	if str(7) == "zlib" {
		plain, err = io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(plain)), maxPage))
		if err != nil {
			return "", fmt.Errorf("paste: decompress: %w", err)
		}
	}
	var msg struct {
		Paste string `json:"paste"`
	}
	if err := json.Unmarshal(plain, &msg); err != nil {
		return "", errors.New("paste: unexpected contents")
	}
	return msg.Paste, nil
}

// jsAData re-encodes adata the way the browser's JSON.stringify did when it
// encrypted: compact, and without escaping "/" (which PHP may have added).
func jsAData(raw json.RawMessage) []byte {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return raw
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return raw
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Decode decodes a PrivateBin key, padded to 32 bytes with leading
// zeros as PrivateBin does.
func base58Decode(s string) ([]byte, error) {
	if s == "" || len(s) > 64 {
		return nil, errors.New("not a key")
	}
	n := new(big.Int)
	for _, c := range s {
		i := strings.IndexRune(base58Alphabet, c)
		if i < 0 {
			return nil, errors.New("not a key")
		}
		n.Mul(n, big.NewInt(58))
		n.Add(n, big.NewInt(int64(i)))
	}
	b := n.Bytes()
	if len(b) > 32 {
		return nil, errors.New("not a key")
	}
	return append(make([]byte, 32-len(b)), b...), nil
}
