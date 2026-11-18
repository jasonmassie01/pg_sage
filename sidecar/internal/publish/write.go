package publish

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// The bundle's files. PackName is the sealed evidence pack of JSONName and
// HTMLName (evidence.Verify reads it); the pack's members are also written
// loose beside it, so a static host serves the page and a reader can run
// sha256sum -c SHA256SUMS.
const (
	JSONName = "outcomes.json"
	HTMLName = "index.html"
	PackName = "outcomes.tar.gz"
)

// Write refusals.
var (
	ErrOutIsFile = errors.New("publish: --out is a file, not a directory")
	ErrOutExists = errors.New("publish: --out is a directory that is not empty; a " +
		"bundle is never written over another")
	ErrNoKey = errors.New("publish: no signing key; a public bundle is always signed")
)

// CheckOut refuses an out that Write would refuse before any work: a
// file, or a directory that is not empty.
func CheckOut(out string) error {
	info, err := os.Stat(out)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("publish: inspect %s: %w", out, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s", ErrOutIsFile, out)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		return fmt.Errorf("publish: read %s: %w", out, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s", ErrOutExists, out)
	}
	return nil
}

// Write seals b signed by key and writes it to out (missing or an empty
// directory). The files are written to a temporary directory beside out
// and renamed into place, so a failure leaves no partial bundle.
func Write(out string, b Bundle, key ed25519.PrivateKey) error {
	if key == nil {
		return ErrNoKey
	}
	if err := CheckOut(out); err != nil {
		return err
	}
	parent := filepath.Dir(filepath.Clean(out))
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		return fmt.Errorf("publish: the parent directory %s of --out does not exist", parent)
	}
	files, err := bundleFiles(b, key)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".pg_sage-outcomes-*")
	if err != nil {
		return fmt.Errorf("publish: create a temporary directory in %s: %w", parent, err)
	}
	if err := fill(tmp, files); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := place(tmp, out); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

// bundleFiles is every file of the bundle by name: the pack and its
// members, which are read back from the pack so the two are identical.
func bundleFiles(b Bundle, key ed25519.PrivateKey) (map[string][]byte, error) {
	doc, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("publish: encode %s: %w", JSONName, err)
	}
	keyID := evidence.KeyID(key.Public().(ed25519.PublicKey))
	pack := evidence.SealFiles(Format, []evidence.File{
		{Name: JSONName, Data: append(doc, '\n')},
		{Name: HTMLName, Data: RenderHTML(b, keyID)}}, key, b.GeneratedAt)
	var buf bytes.Buffer
	if err := pack.WriteTarGz(&buf); err != nil {
		return nil, fmt.Errorf("publish: seal the bundle: %w", err)
	}
	files, err := members(buf.Bytes())
	if err != nil {
		return nil, err
	}
	files[PackName] = buf.Bytes()
	return files, nil
}

func members(pack []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(pack))
	if err != nil {
		return nil, fmt.Errorf("publish: read back the sealed bundle: %w", err)
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("publish: read back the sealed bundle: %w", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("publish: read back %s: %w", h.Name, err)
		}
		files[h.Name] = body
	}
}

func fill(dir string, files map[string][]byte) error {
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			return fmt.Errorf("publish: write %s: %w", name, err)
		}
	}
	return nil
}

// place renames tmp to out, removing out first when it is an empty
// directory (os.Remove refuses a directory that is not empty).
func place(tmp, out string) error {
	if err := os.Remove(out); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("publish: replace the empty directory %s: %w", out, err)
	}
	if err := os.Rename(tmp, out); err != nil {
		return fmt.Errorf("publish: move the bundle into %s: %w", out, err)
	}
	return nil
}
