package workpack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// evid-02c export tier: the local tier without value samples or the
// pseudonym map, as a signed evidence archive that evidence.Verify and
// sha256sum -c both check. It is never unsigned.
// No concurrency test here: Export is a pure function of the pack and key.

var exportTime = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func testSigningKey(t *testing.T, seed byte) ed25519.PrivateKey {
	t.Helper()
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
}

func untar(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name], _ = io.ReadAll(tr)
	}
}

// retar rewrites an archive with one file's first byte changed.
func retar(t *testing.T, files map[string][]byte, edit string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		data = append([]byte(nil), data...)
		if name == edit {
			data[0] ^= 0x01
		}
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))})
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestExport_VerifiesWithTheTrustedKeyAndHoldsNoValueSamples(t *testing.T) {
	key := testSigningKey(t, 3)
	local := modelFixture(t)
	data, err := Export(local, key, exportTime)
	if err != nil {
		t.Fatal(err)
	}
	m, err := evidence.Verify(bytes.NewReader(data), key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("verify the export: %v", err)
	}
	if m.Format != Format || !m.Signed || m.Signer == nil {
		t.Fatalf("export manifest = %+v", m)
	}
	files := untar(t, data)
	for _, f := range local.Files {
		if files[f.Name] == nil {
			t.Errorf("the export has no %s", f.Name)
		}
	}
	for _, want := range []string{"SHA256SUMS", "manifest.json", "manifest.sig"} {
		if files[want] == nil {
			t.Errorf("the export has no %s", want)
		}
	}
	for _, s := range []string{"mcv-marker-7319", "zzzz-bound", "most_common_vals"} {
		if bytes.Contains(files[fileStats], []byte(s)) {
			t.Errorf("the export's stats.json holds %q", s)
		}
	}
	if !bytes.Contains(files[fileTexts], []byte("sk-test-4f9a8b7c6d5e3f21")) {
		t.Error("the export lost the local tier's query text")
	}
	charts := 0
	for name := range files {
		if strings.HasPrefix(name, chartDir) {
			charts++
		}
		if name == "pseudonyms.json" {
			t.Error("the export holds the pseudonym map")
		}
	}
	if charts == 0 {
		t.Error("the export has no charts")
	}
}

func TestExport_AOneByteEditOfAnyFileFailsVerify(t *testing.T) {
	key := testSigningKey(t, 3)
	data, err := Export(modelFixture(t), key, exportTime)
	if err != nil {
		t.Fatal(err)
	}
	files := untar(t, data)
	for name := range files {
		_, err := evidence.Verify(bytes.NewReader(retar(t, files, name)),
			key.Public().(ed25519.PublicKey))
		if err == nil {
			t.Errorf("an edit of %s still verifies", name)
		}
	}
}

func TestExport_AnotherKeyDoesNotVerify(t *testing.T) {
	data, err := Export(modelFixture(t), testSigningKey(t, 3), exportTime)
	if err != nil {
		t.Fatal(err)
	}
	other := testSigningKey(t, 4).Public().(ed25519.PublicKey)
	if _, err := evidence.Verify(bytes.NewReader(data), other); !errors.Is(err,
		evidence.ErrSignature) {
		t.Fatalf("verify under another key: %v, want ErrSignature", err)
	}
}

func TestExport_NoSigningKeyIsRefused(t *testing.T) {
	data, err := Export(modelFixture(t), nil, exportTime)
	if !errors.Is(err, ErrNoSigningKey) || data != nil ||
		!strings.Contains(err.Error(), "no evidence signing key") {
		t.Fatalf("unsigned export: %v (%d bytes)", err, len(data))
	}
	if _, err := Export(nil, testSigningKey(t, 3), exportTime); err == nil {
		t.Fatal("an export of no pack")
	}
}

func TestExport_SameInputsGiveTheSameBytes(t *testing.T) {
	key := testSigningKey(t, 3)
	a, errA := Export(modelFixture(t), key, exportTime)
	b, errB := Export(modelFixture(t), key, exportTime)
	if errA != nil || errB != nil || !bytes.Equal(a, b) {
		t.Fatalf("two exports differ: %v %v", errA, errB)
	}
}
