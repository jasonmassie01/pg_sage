package publish

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// pub-01: the bundle is written whole or not at all, and signed with the
// evidence key so a reader can check it.

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestWrite_SignedBundleVerifiesAndLooseFilesMatchThePack(t *testing.T) {
	key := testKey(t)
	out := filepath.Join(t.TempDir(), "bundle")
	if err := Write(out, sampleBundle(t), key); err != nil {
		t.Fatalf("Write: %v", err)
	}
	pack, err := os.ReadFile(filepath.Join(out, PackName))
	if err != nil {
		t.Fatal(err)
	}
	m, err := evidence.Verify(bytes.NewReader(pack), key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("evidence.Verify: %v", err)
	}
	if m.Format != Format || len(m.Files) != 2 || m.Files[0].Name != HTMLName ||
		m.Files[1].Name != JSONName || !m.Signed {
		t.Fatalf("manifest %+v, want a signed %s of index.html and outcomes.json", m, Format)
	}
	members := readPack(t, pack)
	for _, name := range []string{JSONName, HTMLName, "manifest.json", "manifest.sig",
		"SHA256SUMS"} {
		loose, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("loose %s: %v", name, err)
		}
		if !bytes.Equal(loose, members[name]) {
			t.Errorf("loose %s differs from the pack's", name)
		}
	}
	_, otherKey, _ := ed25519.GenerateKey(nil)
	if _, err := evidence.Verify(bytes.NewReader(pack),
		otherKey.Public().(ed25519.PublicKey)); !errors.Is(err, evidence.ErrSignature) {
		t.Fatalf("verify with another key = %v, want ErrSignature", err)
	}
	keyID := evidence.KeyID(key.Public().(ed25519.PublicKey))
	if !strings.Contains(string(members[HTMLName]), keyID) {
		t.Fatal("index.html does not name the signing key id")
	}
}

func TestWrite_TamperedJSONFailsVerification(t *testing.T) {
	key := testKey(t)
	out := filepath.Join(t.TempDir(), "bundle")
	if err := Write(out, sampleBundle(t), key); err != nil {
		t.Fatal(err)
	}
	pack, _ := os.ReadFile(filepath.Join(out, PackName))
	members := readPack(t, pack)
	members[JSONName] = bytes.Replace(members[JSONName], []byte(`"improved": 21`),
		[]byte(`"improved": 39`), 1)
	if _, err := evidence.Verify(bytes.NewReader(writePack(t, members)),
		key.Public().(ed25519.PublicKey)); !errors.Is(err, evidence.ErrTampered) {
		t.Fatalf("verify an edited outcomes.json = %v, want ErrTampered", err)
	}
}

func TestWrite_RefusesAFileANonEmptyDirectoryAndNoKey(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(file, sampleBundle(t), testKey(t)); !errors.Is(err, ErrOutIsFile) {
		t.Fatalf("Write to a file = %v, want ErrOutIsFile", err)
	}
	full := filepath.Join(dir, "full")
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "old.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(full, sampleBundle(t), testKey(t)); !errors.Is(err, ErrOutExists) {
		t.Fatalf("Write to a non-empty directory = %v, want ErrOutExists", err)
	}
	if err := Write(filepath.Join(dir, "unsigned"), sampleBundle(t), nil); !errors.Is(err,
		ErrNoKey) {
		t.Fatalf("Write without a key = %v, want ErrNoKey", err)
	}
	assertOnly(t, dir, "a-file", "full")
}

func TestWrite_AnEmptyDirectoryIsFilled(t *testing.T) {
	out := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Write(out, sampleBundle(t), testKey(t)); err != nil {
		t.Fatalf("Write to an empty directory: %v", err)
	}
	assertOnly(t, out, HTMLName, JSONName, PackName, "SHA256SUMS", "manifest.json",
		"manifest.sig")
}

func TestWrite_AMissingParentFailsAndLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	err := Write(filepath.Join(dir, "no", "such", "parent"), sampleBundle(t), testKey(t))
	if err == nil || !strings.Contains(err.Error(), "parent") {
		t.Fatalf("Write under a missing parent = %v, want an error naming it", err)
	}
	assertOnly(t, dir)
}

// assertOnly requires dir to hold exactly names (no temporary directory).
func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	if len(got) != len(names) {
		t.Fatalf("%s holds %v, want exactly %v", dir, got, names)
	}
	for _, n := range names {
		if !got[n] {
			t.Fatalf("%s holds %v, want exactly %v", dir, got, names)
		}
	}
}
