package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// audit-10: pg_sage_sidecar evidence verify checks a pack offline, and an
// encryption key alone is enough for packs to be signed.

// No concurrent access tests: the command is a one-shot process; two
// sidecars creating the key at once is evidence.TestLoadOrCreateKey_*.

func evidenceCmdPool(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

// writeEvidencePack builds one action's pack signed by key (nil: unsigned)
// into a file and returns its path.
func writeEvidencePack(t *testing.T, pool *pgxpool.Pool, key ed25519.PrivateKey) string {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type,
		sql_executed) VALUES ('vacuum', 'VACUUM t') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	pack, err := evidence.Build(ctx, evidence.Sources{DB: pool},
		evidence.Scope{Database: "orders", ActionID: id}, key, time.Now())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var buf bytes.Buffer
	if err := pack.WriteTarGz(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	return writeTempFile(t, "pack.tar.gz", buf.Bytes())
}

func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func publicKeyFile(t *testing.T, key ed25519.PrivateKey) string {
	t.Helper()
	text, err := evidence.MarshalPublicKey(key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return writeTempFile(t, "evidence-key.pem", []byte(text))
}

func readTarGzFile(t *testing.T, path string) map[string][]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name], _ = io.ReadAll(tr)
	}
}

// forgeManifest edits manifest.json and regenerates SHA256SUMS, as anyone
// holding the file but not the signing key could.
func forgeManifest(t *testing.T, path string) string {
	t.Helper()
	files := readTarGzFile(t, path)
	edited := bytes.Replace(files["manifest.json"], []byte(`"generator": "pg_sage"`),
		[]byte(`"generator": "forged!"`), 1)
	if bytes.Equal(edited, files["manifest.json"]) {
		t.Fatal("the manifest edit changed nothing")
	}
	files["manifest.json"] = edited
	delete(files, "SHA256SUMS")
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var sums strings.Builder
	for _, n := range names {
		sum := sha256.Sum256(files[n])
		sums.WriteString(hex.EncodeToString(sum[:]) + "  " + n + "\n")
	}
	files["SHA256SUMS"] = []byte(sums.String())
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for n, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	_ = zw.Close()
	return writeTempFile(t, "forged.tar.gz", buf.Bytes())
}

func runEvidence(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := runEvidenceCommand(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// CHECK-24, happy path: the key the sidecar generates (encryption key, no
// env key) signs; verify --public-key exits 0 in either argument order,
// and a forged manifest with regenerated sums exits 1 (ErrSignature).
func TestEvidenceVerifyCLI_GeneratedKeyRoundTrip(t *testing.T) {
	pool := evidenceCmdPool(t, "cmd_evidence_roundtrip")
	c := testConfig()
	got := resolveEvidenceKey(context.Background(), c, pool, testEvidenceKeyring(t, 1))
	if got.key == nil || got.source != evidence.KeySourceGenerated {
		t.Fatalf("resolved key %v source %q, want a generated key", got.key != nil, got.source)
	}
	pack := writeEvidencePack(t, pool, got.key)
	keyFile := publicKeyFile(t, got.key)
	keyID := evidence.KeyID(got.key.Public().(ed25519.PublicKey))
	for _, args := range [][]string{{"verify", pack, "--public-key", keyFile},
		{"verify", "--public-key", keyFile, pack}} {
		code, out, errOut := runEvidence(args...)
		if code != 0 || !strings.Contains(out, keyID) {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
	code, _, errOut := runEvidence("verify", forgeManifest(t, pack), "--public-key", keyFile)
	if code != 1 || !strings.Contains(errOut, evidence.ErrSignature.Error()) {
		t.Fatalf("forged manifest: exit %d, stderr %q", code, errOut)
	}
	code, out, _ := runEvidence("verify", pack)
	if code != 0 || !strings.Contains(out, "--public-key") {
		t.Fatalf("signed pack without a key: exit %d, stdout %q", code, out)
	}
}

// Nil/empty: no encryption key leaves the pack unsigned; it verifies by
// hash with a caveat, and exits 3 (ErrUnsigned) when a key was given.
func TestEvidenceVerifyCLI_UnsignedPack(t *testing.T) {
	pool := evidenceCmdPool(t, "cmd_evidence_unsigned")
	got := resolveEvidenceKey(context.Background(), testConfig(), pool, nil)
	if got.key != nil || got.source != "" {
		t.Fatalf("no encryption key: key %v source %q", got.key != nil, got.source)
	}
	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.evidence_signing_key`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d stored keys without an encryption key (%v)", rows, err)
	}
	pack := writeEvidencePack(t, pool, nil)
	code, out, _ := runEvidence("verify", pack)
	if code != 0 || !strings.Contains(out, evidence.IntegrityHashOnly) {
		t.Fatalf("unsigned, no key: exit %d, stdout %q", code, out)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	code, _, errOut := runEvidence("verify", pack, "--public-key", publicKeyFile(t, other))
	if code != 3 || !strings.Contains(errOut, evidence.ErrUnsigned.Error()) {
		t.Fatalf("unsigned with a key: exit %d, stderr %q", code, errOut)
	}
}

// Invalid input: usage mistakes and unusable keys exit 2 with a message;
// a non-Ed25519 key names its type. A file that is not a pack exits 1.
func TestEvidenceVerifyCLI_InvalidInput(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	ecFile := writeTempFile(t, "ec.pem", pem.EncodeToMemory(&pem.Block{
		Type: "PUBLIC KEY", Bytes: ecDER}))
	junk := writeTempFile(t, "junk.tar.gz", []byte("not a pack"))
	missing := filepath.Join(t.TempDir(), "missing.tar.gz")
	cases := []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "usage"},
		{[]string{"sign", junk}, 2, "verify"},
		{[]string{"verify"}, 2, "pack"},
		{[]string{"verify", junk, junk}, 2, "one pack"},
		{[]string{"verify", junk, "--public-key"}, 2, "public-key"},
		{[]string{"verify", junk, "--public-key", ecFile}, 2, "ECDSA"},
		{[]string{"verify", junk, "--public-key", missing}, 2, "missing.tar.gz"},
		{[]string{"verify", missing}, 2, "missing.tar.gz"},
		{[]string{"verify", junk}, 1, "gzip"},
	}
	for _, c := range cases {
		code, _, errOut := runEvidence(c.args...)
		if code != c.code || !strings.Contains(errOut, c.want) {
			t.Fatalf("%v: exit %d (want %d), stderr %q (want %q)", c.args, code, c.code,
				errOut, c.want)
		}
	}
}

// CHECK-26: docs/security.md documents the subcommand and both manual
// checks, and the documented command is the one this binary dispatches.
func TestEvidenceVerifyIsDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "security.md"))
	if err != nil {
		t.Fatalf("read docs/security.md: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{"pg_sage_sidecar evidence verify", "--public-key",
		"sha256sum -c SHA256SUMS", "openssl pkeyutl -verify", "-rawin",
		"GET /api/v1/audit/evidence/key", "accidental-corruption-only",
		"evidence_signing_key"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/security.md does not mention %q", want)
		}
	}
	if !strings.HasPrefix(evidenceUsage, "usage: pg_sage_sidecar evidence verify") ||
		evidenceSubcommand != "evidence" {
		t.Fatalf("usage %q / subcommand %q differ from the docs", evidenceUsage,
			evidenceSubcommand)
	}
}
