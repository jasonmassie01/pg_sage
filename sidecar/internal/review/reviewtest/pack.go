// Package reviewtest builds workload evidence packs for the review
// generators' tests: the sections a test names, sealed and opened through
// workpack.Decode exactly as a stored pack is, so every evidence id the
// tests cite resolves (or not) by the pack's own index.
package reviewtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
)

// Spec is a pack's content. A nil section is left out of the pack.
type Spec struct {
	Database string
	Window   workpack.Window
	Workload workpack.Workload
	Texts    *workpack.Texts
	Indexes  *workpack.Indexes
	Schema   *workpack.Schema
	Settings *workpack.Settings
}

// Window is the days-long window ending at 2026-11-01 00:00 UTC.
func Window(days int) workpack.Window {
	to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	return workpack.Window{From: to.AddDate(0, 0, -days), To: to}
}

// Build seals s as a pack.
func Build(s Spec) (*workpack.Pack, error) {
	files := map[string]any{
		"header.json": workpack.Header{Format: workpack.Format, Database: s.Database,
			Window: s.Window},
		"coverage.json": workpack.Coverage{ID: "coverage/window/all", Status: "ok"},
		"workload.json": s.Workload,
		"growth.json":   workpack.Growth{DBSize: workpack.Series{ID: "growth/db/size"}},
	}
	if s.Texts != nil {
		files["texts.json"] = *s.Texts
	}
	if s.Indexes != nil {
		files["indexes.json"] = *s.Indexes
	}
	if s.Schema != nil {
		files["schema.json"] = *s.Schema
	}
	if s.Settings != nil {
		files["settings.json"] = *s.Settings
	}
	return seal(s, files)
}

func seal(s Spec, sections map[string]any) (*workpack.Pack, error) {
	names := make([]string, 0, len(sections))
	for n := range sections {
		names = append(names, n)
	}
	sort.Strings(names)
	m := workpack.Manifest{Format: workpack.Format, Database: s.Database, Window: s.Window}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	sum := sha256.New()
	for _, n := range names {
		data, err := json.Marshal(sections[n])
		if err != nil {
			return nil, fmt.Errorf("reviewtest: encode %s: %w", n, err)
		}
		h := sha256.Sum256(data)
		entry := evidence.FileEntry{Name: n, SHA256: hex.EncodeToString(h[:]),
			Bytes: int64(len(data))}
		m.Files = append(m.Files, entry)
		fmt.Fprintf(sum, "%s %s\n", n, entry.SHA256)
		hdr := &tar.Header{Name: n, Mode: 0o644, Size: int64(len(data)),
			ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	m.ContentSHA256 = hex.EncodeToString(sum.Sum(nil))
	return workpack.Decode(buf.Bytes(), m)
}
