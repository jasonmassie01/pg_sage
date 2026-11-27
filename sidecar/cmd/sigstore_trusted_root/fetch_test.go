package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

// These tests run fetchTrustedRoot against a minimal signed TUF
// repository on a loopback httptest server, so the release job's fetch
// path (client options, throwaway cache, target lookup and verification)
// is exercised with no network.
//
// No concurrent access tests: the command is one-shot and every fetch
// makes its own cache. No state transitions: the command has no state
// beyond a single fetch.

const targetName = "trusted_root.json"

// testRepo describes the repository to serve.
type testRepo struct {
	signed      []byte    // target bytes the targets metadata vouches for
	served      []byte    // bytes served for the target; nil serves a 404
	rootExpires time.Time // expiry of the root metadata
	foreignKey  bool      // sign targets.json with a key root does not trust
}

func goodRepo() testRepo {
	raw := benchsig.EmbeddedTrustedRoot()
	return testRepo{signed: raw, served: raw, rootExpires: time.Now().Add(24 * time.Hour)}
}

func newSigner(t *testing.T) (signature.Signer, *metadata.Key) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := signature.LoadED25519Signer(priv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := metadata.KeyFromPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return signer, key
}

func signedBytes[T metadata.Roles](t *testing.T, m *metadata.Metadata[T],
	s signature.Signer) []byte {
	t.Helper()
	if _, err := m.Sign(s); err != nil {
		t.Fatal(err)
	}
	raw, err := m.ToBytes(false)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// files signs the four roles with one key (consistent snapshots, as the
// public-good repository uses) and returns the root and the URL paths.
func (r testRepo) files(t *testing.T) ([]byte, map[string][]byte) {
	t.Helper()
	signer, key := newSigner(t)
	root := metadata.Root(r.rootExpires)
	for _, role := range []string{metadata.ROOT, metadata.TIMESTAMP,
		metadata.SNAPSHOT, metadata.TARGETS} {
		if err := root.Signed.AddKey(key, role); err != nil {
			t.Fatal(err)
		}
	}
	expires := time.Now().UTC().Add(24 * time.Hour)
	targets := metadata.Targets(expires)
	tf, err := metadata.TargetFile().FromBytes(targetName, r.signed)
	if err != nil {
		t.Fatal(err)
	}
	targets.Signed.Targets[targetName] = tf
	targetsSigner := signer
	if r.foreignKey {
		targetsSigner, _ = newSigner(t)
	}
	rootRaw := signedBytes(t, root, signer)
	sum := sha256.Sum256(r.signed)
	files := map[string][]byte{
		"/1.root.json":     rootRaw,
		"/timestamp.json":  signedBytes(t, metadata.Timestamp(expires), signer),
		"/1.snapshot.json": signedBytes(t, metadata.Snapshot(expires), signer),
		"/1.targets.json":  signedBytes(t, targets, targetsSigner),
		"/targets/" + hex.EncodeToString(sum[:]) + "." + targetName: r.served,
	}
	if r.served == nil {
		delete(files, "/targets/"+hex.EncodeToString(sum[:])+"."+targetName)
	}
	return rootRaw, files
}

// serve starts the repository and returns options built the way main
// builds them, pointed at it, plus a count of the requests it answered.
func (r testRepo) serve(t *testing.T) (*tuf.Options, *atomic.Int64) {
	t.Helper()
	rootRaw, files := r.files(t)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		body, ok := files[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return tuf.DefaultOptions().WithRoot(rootRaw).WithRepositoryBaseURL(srv.URL), &hits
}

// isolate points the home and temp directories at fresh directories so
// the test can see where the TUF client wrote its cache.
func isolate(t *testing.T) (home, tmp string) {
	t.Helper()
	home, tmp = t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, v := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(v, tmp)
	}
	return home, tmp
}

func TestFetchTrustedRootFromALocalTUFRepo(t *testing.T) {
	home, tmp := isolate(t)
	opts, hits := goodRepo().serve(t)
	defaultCache := opts.CachePath
	raw, err := fetchTrustedRoot(opts)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !bytes.Equal(raw, benchsig.EmbeddedTrustedRoot()) {
		t.Fatalf("fetched %d bytes, not the served root", len(raw))
	}
	if _, err := benchsig.ParseTrustedRoot(raw); err != nil {
		t.Fatalf("fetched root does not parse: %v", err)
	}
	if hits.Load() < 4 {
		t.Errorf("only %d requests: the repository was not walked", hits.Load())
	}
	if opts.CachePath != defaultCache {
		t.Errorf("caller's options changed: cache %q, was %q", opts.CachePath, defaultCache)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("throwaway cache left behind: %v", left)
	}
	if _, err := os.Stat(filepath.Join(home, ".sigstore")); !os.IsNotExist(err) {
		t.Errorf("fetch used the default cache under home (%v)", err)
	}
}

func TestRunWritesTheRootFetchedThroughTUF(t *testing.T) {
	isolate(t)
	opts, _ := goodRepo().serve(t)
	out := filepath.Join(t.TempDir(), targetName)
	var stderr bytes.Buffer
	code := run([]string{"-o", out}, &stderr,
		func() ([]byte, error) { return fetchTrustedRoot(opts) })
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	got, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(got, benchsig.EmbeddedTrustedRoot()) {
		t.Fatalf("written root differs from the served one (%v)", err)
	}
}

// runKeepsOld runs the command against repo over an existing file and
// requires exit 1, a message holding every want, and the old file
// untouched.
func runKeepsOld(t *testing.T, repo testRepo, wants ...string) {
	t.Helper()
	isolate(t)
	opts, _ := repo.serve(t)
	out := filepath.Join(t.TempDir(), targetName)
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := run([]string{"-o", out}, &stderr,
		func() ([]byte, error) { return fetchTrustedRoot(opts) })
	if code != 1 {
		t.Errorf("exit %d, want 1 (stderr %q)", code, stderr.String())
	}
	for _, want := range wants {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q does not name %q", stderr.String(), want)
		}
	}
	if got, _ := os.ReadFile(out); string(got) != "old" {
		t.Errorf("the old root was replaced with %d bytes", len(got))
	}
}

func TestFetchRefusesATargetWhoseHashDoesNotMatch(t *testing.T) {
	repo := goodRepo()
	i := bytes.IndexByte(repo.signed, '\n')
	if i < 0 {
		t.Fatal("embedded root has no newline to swap")
	}
	// Same length and still a usable root: only the hash can refuse it.
	tampered := append([]byte{}, repo.signed...)
	tampered[i] = ' '
	if _, err := benchsig.ParseTrustedRoot(tampered); err != nil {
		t.Fatalf("tampered root should still parse: %v", err)
	}
	repo.served = tampered
	runKeepsOld(t, repo, "TUF target "+targetName, "hash verification failed")
}

func TestFetchRefusesTargetsSignedByAnUntrustedKey(t *testing.T) {
	repo := goodRepo()
	repo.foreignKey = true
	runKeepsOld(t, repo, "TUF client", "Verifying targets failed")
}

func TestRunRefusesAnEmptyTarget(t *testing.T) {
	repo := goodRepo()
	repo.signed, repo.served = []byte{}, []byte{}
	isolate(t)
	opts, _ := repo.serve(t)
	raw, err := fetchTrustedRoot(opts)
	if err != nil || len(raw) != 0 {
		t.Fatalf("TUF should deliver the signed empty target: %d bytes, %v", len(raw), err)
	}
	runKeepsOld(t, repo, "benchsig: trusted root")
}

func TestFetchTargetNotFoundNamesTheTarget(t *testing.T) {
	repo := goodRepo()
	repo.served = nil
	isolate(t)
	opts, _ := repo.serve(t)
	raw, err := fetchTrustedRoot(opts)
	if err == nil || !strings.Contains(err.Error(), "TUF target "+targetName) {
		t.Fatalf("want an error naming %s, got %v", targetName, err)
	}
	if raw != nil {
		t.Errorf("returned %d bytes with the error", len(raw))
	}
}

func TestFetchCacheThatCannotBeCreated(t *testing.T) {
	opts, hits := goodRepo().serve(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(v, filepath.Join(blocker, "sub"))
	}
	_, err := fetchTrustedRoot(opts)
	if err == nil || !strings.Contains(err.Error(), "TUF cache") {
		t.Fatalf("want a TUF cache error, got %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("fetched %d times without a cache", n)
	}
}

func TestFetchRefusesExpiredRootMetadata(t *testing.T) {
	repo := goodRepo()
	repo.rootExpires = time.Now().Add(-time.Minute)
	isolate(t)
	opts, _ := repo.serve(t)
	_, err := fetchTrustedRoot(opts)
	if err == nil || !strings.Contains(err.Error(), "TUF client") ||
		!strings.Contains(err.Error(), "expired") {
		t.Fatalf("want an expired-root TUF client error, got %v", err)
	}
}

func TestFetchAcceptsRootMetadataJustBeforeExpiry(t *testing.T) {
	repo := goodRepo()
	repo.rootExpires = time.Now().Add(time.Minute)
	isolate(t)
	opts, _ := repo.serve(t)
	raw, err := fetchTrustedRoot(opts)
	if err != nil || !bytes.Equal(raw, repo.served) {
		t.Fatalf("a root valid for one more minute was refused: %d bytes, %v", len(raw), err)
	}
}

func TestFetchTrustedRootNilOptions(t *testing.T) {
	raw, err := fetchTrustedRoot(nil)
	if err == nil || !strings.Contains(err.Error(), "TUF options") || raw != nil {
		t.Fatalf("want a TUF options error and no bytes, got %d bytes, %v", len(raw), err)
	}
}
