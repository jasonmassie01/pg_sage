package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// pub-01: docs/outcomes-sample is the bundle `pg_sage publish outcomes
// --sample` writes, byte for byte; a test fails when it is stale.

func TestSampleBundle_IsCommittedAndCurrent(t *testing.T) {
	out := filepath.Join(t.TempDir(), "sample")
	if err := Write(out, SampleBundle(), SampleKey()); err != nil {
		t.Fatalf("Write the sample: %v", err)
	}
	committed := filepath.Join("..", "..", "..", "docs", "outcomes-sample")
	for _, name := range []string{JSONName, HTMLName, PackName, "manifest.json",
		"manifest.sig", "SHA256SUMS"} {
		want, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(committed, name))
		if err != nil {
			t.Fatalf("read the committed sample %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("docs/outcomes-sample/%s is stale: run `go run ./cmd/pg_sage_sidecar "+
				"publish outcomes --sample --out ../docs/outcomes-sample` from sidecar/ "+
				"(after removing the directory)", name)
		}
	}
}

func TestSampleBundle_IsMarkedSampleAndVerifiesWithTheSampleKey(t *testing.T) {
	b := SampleBundle()
	if !b.Sample || b.Totals == nil || b.Totals.Changes != 40 || b.Verifier == nil {
		t.Fatalf("sample bundle %+v, want a marked sample of 40 changes with a rate", b)
	}
	out := filepath.Join(t.TempDir(), "sample")
	if err := Write(out, b, SampleKey()); err != nil {
		t.Fatal(err)
	}
	pack, _ := os.ReadFile(filepath.Join(out, PackName))
	pub := SampleKey().Public().(ed25519.PublicKey)
	if _, err := evidence.Verify(bytes.NewReader(pack), pub); err != nil {
		t.Fatalf("the sample does not verify with the sample key: %v", err)
	}
	html, _ := os.ReadFile(filepath.Join(out, HTMLName))
	if !strings.Contains(string(html), "Sample data") {
		t.Fatal("the sample page does not say it is sample data")
	}
}

func readPack(t *testing.T, pack []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(pack))
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
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name] = body
	}
}

func writePack(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644,
			Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
