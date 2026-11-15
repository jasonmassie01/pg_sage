package workpack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// Where each kind of fact was read from.
const (
	sourceRollup     = "sage.query_rollup_hourly, sage.query_rollup_daily"
	sourceCoverage   = "sage.query_rollup_coverage"
	sourceSystem     = "sage.snapshots (system)"
	sourceTables     = "sage.snapshots (tables)"
	coverageEvidence = "coverage/window/all"
)

// assemble renders h as the pack of req.
func assemble(req Request, plan dayPlan, h history) (*Pack, error) {
	w := req.Window.UTC()
	wl, fams := buildWorkload(req, plan, h)
	c := h.coverage
	parts := []part{
		{fileHeader, Header{Format: Format, Database: req.Database, Window: w,
			Timezone: req.location().String(), PGVersion: req.PGVersion,
			Platform: req.Platform, Capabilities: req.Capabilities,
			HourlyRetentionDays: req.HourlyDays, Days: plan.days}},
		{fileCoverage, Coverage{ID: coverageEvidence, Status: c.Status,
			CoveredExecShare: c.CoveredExecShare, EpochBreaks: c.EpochBreaks, Hours: c.Hours,
			HoursMissing: c.HoursMissing, DeallocGrowth: c.DeallocGrowth,
			CountersOnlyHours: c.CountersOnlyHours}},
		{fileWorkload, wl},
		{fileGrowth, buildGrowth(req, plan, h, fams)},
	}
	parts = append(parts, catalogParts(req, plan, h, wl)...)
	files := make([]evidence.File, 0, len(parts))
	for _, part := range parts {
		data, err := canonical(part.v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", part.name, err)
		}
		files = append(files, evidence.File{Name: part.name, Data: data})
	}
	return seal(req, w, files)
}

// seal hashes, manifests and packs the section files, refusing a body
// over the bound with the largest section named.
func seal(req Request, w Window, files []evidence.File) (*Pack, error) {
	sealed := evidence.SealFiles(Format, files, nil, req.Now)
	sums := map[string]string{}
	for _, e := range sealed.Manifest.Files {
		sums[e.Name] = e.SHA256
	}
	body, err := tarGz(sealed.Files)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > req.maxBody() {
		largest := sealed.Files[0]
		for _, f := range sealed.Files {
			if len(f.Data) > len(largest.Data) {
				largest = f
			}
		}
		return nil, fmt.Errorf("%w: the gzip body is %d bytes, at most %d; the largest "+
			"section is %s (%d bytes)", ErrPackTooLarge, len(body), req.maxBody(),
			largest.Name, len(largest.Data))
	}
	m := Manifest{Format: Format, ContentSHA256: contentSHA256(sums),
		Database: req.Database, Window: w, Files: sealed.Manifest.Files,
		Completeness: completenessOf(sealed.Files)}
	p := &Pack{Format: Format, ContentSHA256: m.ContentSHA256, Window: w,
		Files: sealed.Files, Body: body, Manifest: m}
	if err := p.index(); err != nil {
		return nil, err
	}
	return p, nil
}

// index decodes the sections and lists every fact by its evidence id.
func (p *Pack) index() error {
	var cov Coverage
	var wl Workload
	var g Growth
	for name, dst := range map[string]any{fileCoverage: &cov, fileWorkload: &wl,
		fileGrowth: &g} {
		if err := json.Unmarshal(p.File(name), dst); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrCorrupt, name, err)
		}
	}
	p.facts = map[string]Fact{}
	add := func(id, section, unit, source string, v any) error {
		raw, err := canonical(v)
		if err != nil {
			return err
		}
		p.facts[id] = Fact{ID: id, Section: section, Value: json.RawMessage(raw),
			Unit: unit, Window: p.Window, Source: source}
		return nil
	}
	if err := add(cov.ID, "coverage", "share of statement time", sourceCoverage,
		cov); err != nil {
		return err
	}
	for _, fam := range wl.Families {
		if err := add(fam.ID, "workload", "calls, ms", sourceRollup, fam); err != nil {
			return err
		}
		for _, q := range fam.QueryIDs {
			if err := add(q.ID, "workload", "calls, ms", sourceRollup, q); err != nil {
				return err
			}
		}
	}
	if err := p.indexGrowth(g, add); err != nil {
		return err
	}
	return p.indexCatalog(add)
}

func (p *Pack) indexGrowth(g Growth,
	add func(id, section, unit, source string, v any) error) error {
	for _, fg := range g.Workload {
		if err := add(fg.ID, "growth", "per week", sourceRollup, fg); err != nil {
			return err
		}
	}
	if err := add(g.DBSize.ID, "growth", "bytes/day", sourceSystem, g.DBSize); err != nil {
		return err
	}
	for _, t := range g.Tables {
		if err := add(t.ID, "growth", "bytes/day", sourceTables, t); err != nil {
			return err
		}
	}
	return nil
}

// sha256Hex is the hex SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
