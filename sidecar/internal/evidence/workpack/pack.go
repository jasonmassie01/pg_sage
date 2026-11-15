package workpack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// Pack is a built or stored workload evidence pack.
type Pack struct {
	Format        string
	ContentSHA256 string
	Window        Window
	// Files are the section files in name order; Body is their gzip tar.
	Files    []evidence.File
	Body     []byte
	Manifest Manifest
	facts    map[string]Fact
}

// Manifest is what sage.workload_packs.manifest holds: the pack's format,
// content hash, database, window and each section's entry
// (evidence.SealFiles: name, SHA-256, bytes).
type Manifest struct {
	Format        string               `json:"format"`
	ContentSHA256 string               `json:"content_sha256"`
	Database      string               `json:"database"`
	Window        Window               `json:"window"`
	Files         []evidence.FileEntry `json:"files"`
}

// Fact is one resolved evidence id: its section, value (the entry's
// canonical JSON), unit, the pack's window and the table it was read from.
type Fact struct {
	ID      string          `json:"id"`
	Section string          `json:"section"`
	Value   json.RawMessage `json:"value"`
	Unit    string          `json:"unit"`
	Window  Window          `json:"window"`
	Source  string          `json:"source"`
}

// File is the named section's bytes (nil when absent).
func (p *Pack) File(name string) []byte {
	for _, f := range p.Files {
		if f.Name == name {
			return f.Data
		}
	}
	return nil
}

// Header is the pack's decoded header section.
func (p *Pack) Header() (Header, error) {
	var h Header
	if err := json.Unmarshal(p.File(fileHeader), &h); err != nil {
		return Header{}, fmt.Errorf("%w: %s: %v", ErrCorrupt, fileHeader, err)
	}
	return h, nil
}

// IDs lists every evidence id the pack holds, sorted.
func (p *Pack) IDs() []string {
	out := make([]string, 0, len(p.facts))
	for id := range p.facts {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// sections are the id-bearing sections of a pack.
var sections = map[string]bool{"workload": true, "coverage": true, "growth": true}

// Resolve returns the fact id names in p, or ErrUnknownEvidence for an id
// p does not hold: a malformed one (not section/kind/key, an empty part,
// "..", an unknown section) included.
func Resolve(p *Pack, id string) (Fact, error) {
	parts := strings.SplitN(id, "/", 3)
	if p == nil || len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" ||
		strings.Contains(id, "..") || !sections[parts[0]] {
		return Fact{}, fmt.Errorf("%w: %q", ErrUnknownEvidence, id)
	}
	f, ok := p.facts[id]
	if !ok {
		return Fact{}, fmt.Errorf("%w: %q is not in pack %s", ErrUnknownEvidence, id,
			p.ContentSHA256)
	}
	return f, nil
}

// tarGz writes files (name order) as a gzip tar with fixed times.
func tarGz(files []evidence.File) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		h := &tar.Header{Name: f.Name, Mode: 0o644, Size: int64(len(f.Data)),
			ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return nil, fmt.Errorf("workpack: write %s: %w", f.Name, err)
		}
		if _, err := tw.Write(f.Data); err != nil {
			return nil, fmt.Errorf("workpack: write %s: %w", f.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("workpack: close the tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("workpack: close the gzip: %w", err)
	}
	return buf.Bytes(), nil
}

// maxUnpacked bounds what Decode reads from a body.
const maxUnpacked = 256 << 20

// untarGz reads a body's files.
func untarGz(body []byte) ([]evidence.File, error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	tr := tar.NewReader(io.LimitReader(gz, maxUnpacked))
	var files []evidence.File
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("%w: read %s: %v", ErrCorrupt, h.Name, err)
		}
		files = append(files, evidence.File{Name: h.Name, Data: data})
	}
}

// Decode opens a stored body against its manifest: every file must match
// its entry and the files must hash to the manifest's content_sha256,
// else ErrCorrupt.
func Decode(body []byte, m Manifest) (*Pack, error) {
	files, err := untarGz(body)
	if err != nil {
		return nil, err
	}
	want := map[string]string{}
	for _, e := range m.Files {
		want[e.Name] = e.SHA256
	}
	sums := map[string]string{}
	for _, f := range files {
		sums[f.Name] = sha256Hex(f.Data)
		if want[f.Name] != sums[f.Name] {
			return nil, fmt.Errorf("%w: %s does not match its manifest entry", ErrCorrupt,
				f.Name)
		}
	}
	if len(sums) != len(want) || contentSHA256(sums) != m.ContentSHA256 {
		return nil, fmt.Errorf("%w: the files do not hash to %s", ErrCorrupt,
			m.ContentSHA256)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	p := &Pack{Format: m.Format, ContentSHA256: m.ContentSHA256, Window: m.Window,
		Files: files, Body: body, Manifest: m}
	if err := p.index(); err != nil {
		return nil, err
	}
	return p, nil
}
