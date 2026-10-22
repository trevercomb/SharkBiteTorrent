// Command releasekey manages the key that signs releases, so the in-app
// updater only installs releases the maintainer signed, even if the GitHub
// account or a release's files are tampered with.
//
// The private key never goes into git or GitHub: it's kept on the
// maintainer's computer as a password-protected minisign key, plus a
// password-protected backup kept offline.
//
//	go run ./cmd/releasekey init              new key; writes release.pub (commit it)
//	go run ./cmd/releasekey backup <file>     password-protected copy for safekeeping
//	go run ./cmd/releasekey restore <file>    load a backup onto this computer
//	go run ./cmd/releasekey publish <tag>     sign a draft release's SHA256SUMS.txt and publish it
//
// Signatures are standard minisign, so anyone can check a release with:
//
//	minisign -Vm SHA256SUMS.txt -p release.pub
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"aead.dev/minisign"
	"golang.org/x/term"

	"sharkbite/internal/secret"
)

const (
	repo        = "trevercomb/SharkBiteTorrent"
	pubKeyFile  = "release.pub" // in the repo root; embedded in the app
	sumsAsset   = "SHA256SUMS.txt"
	sigAsset    = "SHA256SUMS.txt.minisig"
	commentForm = "SharkBiteTorrent %s" // trusted comment; the updater checks it
)

func keyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".sharkbite-release", "minisign.key")
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch arg := func(i int) string {
		if len(os.Args) <= i {
			usage()
		}
		return os.Args[i]
	}; os.Args[1] {
	case "init":
		err = initKey()
	case "backup":
		err = backup(arg(2))
	case "restore":
		err = restore(arg(2))
	case "publish":
		err = publish(arg(2))
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "releasekey:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: releasekey init | backup <file> | restore <file> | publish <tag>")
	os.Exit(2)
}

// The key is stored as a standard password-protected minisign key (the same
// format as a backup), so nothing else on the computer can use it: not the
// torrent app, which shares the OS keyring entry older versions used.

// loadKey asks for the key's password. A key saved by an older version
// (encrypted with the app's keyring key) is moved to a password once.
func loadKey() (minisign.PrivateKey, error) {
	var k minisign.PrivateKey
	data, err := os.ReadFile(keyPath())
	if err != nil {
		return k, fmt.Errorf("no signing key on this computer (%v); run init or restore", err)
	}
	if !bytes.HasPrefix(data, []byte("untrusted comment:")) {
		plain, err := secret.ReadFile(keyPath())
		if err != nil {
			return k, err
		}
		if err := k.UnmarshalText(plain); err != nil {
			return k, err
		}
		fmt.Fprintln(os.Stderr, "The signing key is now protected by its own password. Choose one (you'll type it for every publish).")
		if err := saveKey(k); err != nil {
			return k, err
		}
		return k, nil
	}
	p, err := password("Signing key password: ")
	if err != nil {
		return k, err
	}
	return minisign.DecryptKey(p, data)
}

// saveKey asks for a new password and writes the encrypted key.
func saveKey(k minisign.PrivateKey) error {
	p, err := newPassword("New password for the signing key: ")
	if err != nil {
		return err
	}
	enc, err := minisign.EncryptKey(p, k)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath()), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(keyPath(), enc)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// newPassword asks twice for a password of at least 12 characters.
func newPassword(prompt string) (string, error) {
	p1, err := password(prompt)
	if err != nil {
		return "", err
	}
	if len(p1) < 12 {
		return "", errors.New("use at least 12 characters")
	}
	p2, err := password("Again: ")
	if err != nil {
		return "", err
	}
	if p1 != p2 {
		return "", errors.New("passwords don't match")
	}
	return p1, nil
}

func writePub(k minisign.PrivateKey) error {
	pub := k.Public().(minisign.PublicKey)
	text, err := pub.MarshalText()
	if err != nil {
		return err
	}
	return os.WriteFile(pubKeyFile, append(bytes.TrimSpace(text), '\n'), 0o644)
}

func initKey() error {
	if _, err := os.Stat(keyPath()); err == nil {
		return fmt.Errorf("a key already exists at %s; refusing to replace it", keyPath())
	}
	_, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := saveKey(priv); err != nil {
		return err
	}
	if err := writePub(priv); err != nil {
		return err
	}
	fmt.Printf("Key %X saved (password-protected) to %s\nPublic key written to %s; commit it.\nNow make a backup: go run ./cmd/releasekey backup <file>\n", priv.ID(), keyPath(), pubKeyFile)
	return nil
}

func password(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

func backup(file string) error {
	k, err := loadKey()
	if err != nil {
		return err
	}
	p1, err := newPassword("Backup password: ")
	if err != nil {
		return err
	}
	enc, err := minisign.EncryptKey(p1, k)
	if err != nil {
		return err
	}
	if err := os.WriteFile(file, enc, 0o600); err != nil {
		return err
	}
	// Prove the backup opens before anyone relies on it.
	if back, err := minisign.DecryptKey(p1, enc); err != nil || !back.Equal(k) {
		return errors.New("backup didn't verify; don't use it")
	}
	fmt.Printf("Backup written to %s (minisign format, password-protected). Keep it and the password somewhere safe, off this computer.\n", file)
	return nil
}

func restore(file string) error {
	if _, err := os.Stat(keyPath()); err == nil {
		return fmt.Errorf("a key already exists at %s; move it away first if you really mean to replace it", keyPath())
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	p, err := password("Backup password: ")
	if err != nil {
		return err
	}
	k, err := minisign.DecryptKey(p, data)
	if err != nil {
		return err
	}
	if err := saveKey(k); err != nil {
		return err
	}
	fmt.Printf("Key %X restored to %s\n", k.ID(), keyPath())
	return nil
}

var tagPattern = regexp.MustCompile(`^v[0-9]+(\.[0-9]+){1,3}(-[0-9A-Za-z.]+)?$`)

// publish signs the checksums of a draft release made by the release
// workflow, uploads the signature, and publishes the release.
func publish(tag string) error {
	if !tagPattern.MatchString(tag) {
		return fmt.Errorf("not a release tag: %q", tag)
	}
	// The commit this computer's tag points to is what we trust; the draft's
	// files must provably come from it. This tool must be that code too
	// (go run builds it from the checkout), so the checkout must be the tag,
	// unmodified.
	commit, err := gitOut("rev-parse", tag+"^{commit}")
	if err != nil {
		return fmt.Errorf("tag %s isn't in this repository: %w", tag, err)
	}
	if head, err := gitOut("rev-parse", "HEAD"); err != nil || head != commit {
		return fmt.Errorf("check out the tag first (git checkout %s), so this tool runs the released code", tag)
	}
	if dirty, err := gitOut("status", "--porcelain"); err != nil || dirty != "" {
		return errors.New("the checkout has local changes; publish from a clean checkout of the tag")
	}
	k, err := loadKey()
	if err != nil {
		return err
	}
	pubText, err := os.ReadFile(pubKeyFile)
	if err != nil {
		return fmt.Errorf("run from the repo root: %w", err)
	}
	var pub minisign.PublicKey
	if err := pub.UnmarshalText(bytes.TrimSpace(pubText)); err != nil {
		return err
	}
	if !pub.Equal(k.Public()) {
		return errors.New("this computer's key doesn't match release.pub")
	}

	dir, err := os.MkdirTemp("", "releasekey-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := gh("release", "download", tag, "--repo", repo, "--dir", dir); err != nil {
		return err
	}
	sums, err := checkDraft(dir, tag, commit)
	if err != nil {
		return fmt.Errorf("not signing %s: %w", tag, err)
	}
	// The Microsoft Store package, checked the same way before anything is
	// published. It isn't part of the GitHub release (Partner Center signs
	// it), so it comes from the release workflow run's artifacts. Betas
	// (v0.3.0-beta.1) have none: the Store gets releases only.
	beta := strings.Contains(tag, "-")
	var msix string
	if !beta {
		if msix, err = storePackage(dir, tag, commit); err != nil {
			return fmt.Errorf("not publishing %s: Store package: %w", tag, err)
		}
	}
	fmt.Printf("Signing %s for %s:\n%s", sumsAsset, tag, sums)
	sig := minisign.SignWithComments(k, sums, fmt.Sprintf(commentForm, tag), "signed with the Shark BiteTorrent release key")
	if !minisign.Verify(pub, sums, sig) {
		return errors.New("signature didn't verify")
	}
	sigPath := filepath.Join(dir, sigAsset)
	if err := os.WriteFile(sigPath, sig, 0o644); err != nil {
		return err
	}
	if err := gh("release", "upload", tag, sigPath, "--repo", repo, "--clobber"); err != nil {
		return err
	}
	// A pre-release (v0.3.0-rc1) mustn't become "latest": the app's update
	// check follows /releases/latest.
	flags := []string{"release", "edit", tag, "--repo", repo, "--draft=false", "--latest"}
	if beta {
		flags = []string{"release", "edit", tag, "--repo", repo, "--draft=false", "--prerelease", "--latest=false"}
	}
	if err := gh(flags...); err != nil {
		return err
	}
	fmt.Printf("Published %s with a signed %s.\n", tag, sumsAsset)
	if beta {
		fmt.Println("A beta: published as a pre-release, offered only to apps on the beta update channel. Nothing for the Store.")
		return nil
	}

	home, _ := os.UserHomeDir()
	dst := filepath.Join(home, "Downloads", filepath.Base(msix))
	data, err := os.ReadFile(msix)
	if err == nil {
		err = os.WriteFile(dst, data, 0o644)
	}
	if err != nil {
		return fmt.Errorf("published, but couldn't copy the Store package to Downloads: %w", err)
	}
	fmt.Printf("Verified Store package: %s\nUpload it in Partner Center: the app → Packages → drag it in → Submit.\n", dst)
	return nil
}

// storePackage downloads the .msix built by the release workflow run for
// tag and checks its build provenance like the release files.
func storePackage(dir, tag, commit string) (string, error) {
	out, err := exec.Command("gh", "run", "list", "--repo", repo, "--workflow", "release.yml",
		"--branch", tag, "--event", "push", "--limit", "1", "--json", "databaseId", "--jq", ".[0].databaseId").Output()
	runID := strings.TrimSpace(string(out))
	if err != nil || runID == "" {
		return "", fmt.Errorf("no release workflow run found for %s", tag)
	}
	msixDir := filepath.Join(dir, "msix")
	if err := gh("run", "download", runID, "--repo", repo, "--name", "msix", "--dir", msixDir); err != nil {
		return "", err
	}
	found, _ := filepath.Glob(filepath.Join(msixDir, "*.msix"))
	if len(found) != 1 {
		return "", fmt.Errorf("expected one .msix in the run's artifact, found %d", len(found))
	}
	if err := verifyProvenance(found[0], tag, commit); err != nil {
		return "", err
	}
	fmt.Printf("  ok %s: built by release.yml from %.12s\n", filepath.Base(found[0]), commit)
	return found[0], nil
}

// verifyProvenance checks that GitHub's build provenance shows path was built
// by this repo's release workflow, on a GitHub-hosted runner, from exactly
// commit (what the local tag points to).
func verifyProvenance(path, tag, commit string) error {
	cmd := exec.Command("gh", "attestation", "verify", path, "--repo", repo,
		"--source-digest", commit,
		"--source-ref", "refs/tags/"+tag,
		"--signer-workflow", repo+"/.github/workflows/release.yml",
		"--deny-self-hosted-runners")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s has no valid build provenance for %s at %.12s:\n%s", filepath.Base(path), tag, commit, out)
	}
	return nil
}

// checkDraft verifies the downloaded draft before anything is signed:
//   - every file has GitHub's build provenance showing it was built by this
//     repo's release workflow, on a GitHub-hosted runner, from exactly the
//     commit the local tag points to (so files swapped into the draft, or
//     built from other code or a modified workflow, are refused);
//   - SHA256SUMS.txt lists exactly those files, with the hashes computed here.
//
// It returns the checksums file's contents.
func checkDraft(dir, tag, commit string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	actual := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == sigAsset {
			continue
		}
		path := filepath.Join(dir, name)
		if err := verifyProvenance(path, tag, commit); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		actual[name] = hex.EncodeToString(sum[:])
		fmt.Printf("  ok %s: built by release.yml from %.12s\n", name, commit)
	}

	sums, err := os.ReadFile(filepath.Join(dir, sumsAsset))
	if err != nil {
		return nil, err
	}
	listed := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, fmt.Errorf("unexpected line in %s: %q", sumsAsset, line)
		}
		listed[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
	}
	for name, h := range actual {
		if name == sumsAsset {
			continue
		}
		if listed[name] != h {
			return nil, fmt.Errorf("%s: hash %s doesn't match %s (%q)", name, h, sumsAsset, listed[name])
		}
	}
	for name := range listed {
		if _, ok := actual[name]; !ok {
			return nil, fmt.Errorf("%s lists %s, which isn't in the release", sumsAsset, name)
		}
	}
	return sums, nil
}

func gitOut(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(out)), err
}

func gh(args ...string) error {
	cmd := exec.Command("gh", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh %s: %w", strings.Join(args[:2], " "), err)
	}
	return nil
}
