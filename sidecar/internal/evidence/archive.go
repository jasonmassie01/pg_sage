package evidence

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Pack file names written beside the content files.
const (
	manifestName  = "manifest.json"
	signatureName = "manifest.sig"
	sumsName      = "SHA256SUMS"
)

// Verification failures.
var (
	ErrTampered  = errors.New("evidence: a file does not match its recorded hash")
	ErrSignature = errors.New("evidence: the manifest signature does not verify")
	ErrUnsigned  = errors.New("evidence: the pack is not signed")
)

// maxPackBytes bounds what Verify reads.
const maxPackBytes = 512 << 20

// WriteTarGz writes the pack: content files, manifest.json, manifest.sig
// (when signed) and SHA256SUMS over all of them.
func (p *Pack) WriteTarGz(w io.Writer) error {
	manifest, err := json.MarshalIndent(p.Manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("evidence: encode manifest: %w", err)
	}
	files := append([]File(nil), p.Files...)
	files = append(files, File{Name: manifestName, Data: manifest})
	if p.key != nil {
		sig := base64.StdEncoding.EncodeToString(ed25519.Sign(p.key, manifest))
		files = append(files, File{Name: signatureName, Data: []byte(sig + "\n")})
	}
	files = append(files, File{Name: sumsName, Data: sums(files)})
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		h := &tar.Header{Name: f.Name, Mode: 0o644, Size: int64(len(f.Data)),
			ModTime: p.Manifest.CreatedAt, Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("evidence: write %s: %w", f.Name, err)
		}
		if _, err := tw.Write(f.Data); err != nil {
			return fmt.Errorf("evidence: write %s: %w", f.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("evidence: close tar: %w", err)
	}
	return gz.Close()
}

// sums renders sha256sum -c lines for files, sorted by name.
func sums(files []File) []byte {
	sorted := append([]File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	for _, f := range sorted {
		sum := sha256.Sum256(f.Data)
		b.WriteString(hex.EncodeToString(sum[:]) + "  " + f.Name + "\n")
	}
	return []byte(b.String())
}

// Verify checks a pack: every file against SHA256SUMS and the manifest,
// and, when trusted is set, the manifest's signature by that key.
func Verify(r io.Reader, trusted ed25519.PublicKey) (Manifest, error) {
	files, err := readArchive(r)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(files[manifestName], &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest.json is missing or invalid", ErrTampered)
	}
	if err := checkSums(files); err != nil {
		return Manifest{}, err
	}
	if err := checkManifest(m, files); err != nil {
		return Manifest{}, err
	}
	if trusted == nil {
		return m, nil
	}
	sig, ok := files[signatureName]
	if !ok {
		return Manifest{}, ErrUnsigned
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(trusted, files[manifestName], raw) ||
		m.Signer == nil || m.Signer.KeyID != KeyID(trusted) {
		return Manifest{}, ErrSignature
	}
	return m, nil
}

func readArchive(r io.Reader) (map[string][]byte, error) {
	gz, err := gzip.NewReader(io.LimitReader(r, maxPackBytes))
	if err != nil {
		return nil, fmt.Errorf("evidence: not a gzip archive: %w", err)
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("evidence: read archive: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxPackBytes))
		if err != nil {
			return nil, fmt.Errorf("evidence: read %s: %w", h.Name, err)
		}
		files[h.Name] = body
	}
}

// checkSums requires SHA256SUMS to list every other file with its hash.
func checkSums(files map[string][]byte) error {
	listed := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(files[sumsName])), "\n") {
		sum, name, ok := strings.Cut(line, "  ")
		body, present := files[name]
		got := sha256.Sum256(body)
		if !ok || !present || hex.EncodeToString(got[:]) != sum {
			return fmt.Errorf("%w: %s", ErrTampered, name)
		}
		listed[name] = true
	}
	for name := range files {
		if name != sumsName && !listed[name] {
			return fmt.Errorf("%w: %s is not listed in SHA256SUMS", ErrTampered, name)
		}
	}
	return nil
}

// checkManifest requires every manifest entry to match its file.
func checkManifest(m Manifest, files map[string][]byte) error {
	for _, f := range m.Files {
		body, ok := files[f.Name]
		sum := sha256.Sum256(body)
		if !ok || hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(body)) != f.Bytes {
			return fmt.Errorf("%w: %s", ErrTampered, f.Name)
		}
	}
	return nil
}

// KeyID names a public key: the first 16 hex characters of its SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// LoadSigningKey parses an Ed25519 private key in PKCS#8 PEM.
func LoadSigningKey(pemText string) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, fmt.Errorf("evidence: signing key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("evidence: signing key: %w", err)
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("evidence: signing key is %T, want Ed25519", key)
	}
	return ed, nil
}

// MarshalSigningKey renders key as PKCS#8 PEM.
func MarshalSigningKey(key ed25519.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("evidence: marshal signing key: %w", err)
	}
	var b bytes.Buffer
	if err := pem.Encode(&b, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		return "", fmt.Errorf("evidence: marshal signing key: %w", err)
	}
	return b.String(), nil
}
