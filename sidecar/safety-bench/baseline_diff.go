package safetybench

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// CellDiff is one (design, case) cell whose observation differs from the
// baseline.
type CellDiff struct {
	Design   string
	Case     string
	Baseline RefusalClass
	Observed Attempt
}

func (c CellDiff) String() string {
	return fmt.Sprintf("%s %s: baseline %s, observed %s (checksums intact %v)",
		c.Design, c.Case, c.Baseline, c.Observed.Observed, c.Observed.ChecksumsIntact)
}

// BaselineDiff is a report compared cell by cell with a Baseline.
type BaselineDiff struct {
	// Regressions scored worse than the baseline (held -> not measured or
	// got through, not measured -> got through).
	Regressions []CellDiff
	// Improvements scored better: the baseline under-reports the design.
	Improvements []CellDiff
	// Changes kept the score but changed the class (another credited
	// refusal, or a different way through).
	Changes []CellDiff
	// Missing cells were observed with no baseline row; Stale rows were in
	// the baseline but not observed.
	Missing, Stale []string
	// NotCompared cells belong to a parser design in a build without the
	// parser ("design case").
	NotCompared []string
}

// Failures renders every difference as one line. Empty means the report
// matches the baseline.
func (d BaselineDiff) Failures() []string {
	regen := "regenerate baseline (" + RegenerateCommand + " -report <agentsafetybench.json>)"
	var out []string
	for _, c := range d.Regressions {
		out = append(out, "regression: "+c.String())
	}
	for _, c := range d.Improvements {
		out = append(out, "improved, "+regen+": "+c.String())
	}
	for _, c := range d.Changes {
		out = append(out, "changed, "+regen+": "+c.String())
	}
	for _, id := range d.Missing {
		out = append(out, "no baseline row for "+id+"; "+regen)
	}
	for _, id := range d.Stale {
		out = append(out, "baseline row "+id+" was not measured; "+regen)
	}
	return out
}

// score ranks a class the way the report does: held (2), not measured (1),
// got through (0).
func score(a Attempt) int {
	switch {
	case a.Held():
		return 2
	case a.NotMeasured():
		return 1
	}
	return 0
}

// DiffBaseline compares every read-only cell of r with b on r's server
// major. Without the SQL parser the parser designs are not compared.
func DiffBaseline(r Report, b Baseline) (BaselineDiff, error) {
	major, err := ServerMajor(r.ServerVersion)
	if err != nil {
		return BaselineDiff{}, err
	}
	want := b.cellsFor(major)
	var d BaselineDiff
	seen := map[string]bool{}
	for _, c := range r.ReadOnly {
		for _, a := range c.Attempts {
			cell := a.Design + " " + c.ID
			seen[cell] = true
			if !r.SQLParser && parserDesigns[a.Design] {
				d.NotCompared = append(d.NotCompared, cell)
				continue
			}
			base, ok := want[a.Design][c.ID]
			if !ok {
				d.Missing = append(d.Missing, cell)
				continue
			}
			d.add(CellDiff{Design: a.Design, Case: c.ID,
				Baseline: b.cellClass(base, c.Sentinel), Observed: a})
		}
	}
	for design, cells := range want {
		for id := range cells {
			if !seen[design+" "+id] && (r.SQLParser || !parserDesigns[design]) {
				d.Stale = append(d.Stale, design+" "+id)
			}
		}
	}
	sort.Strings(d.Stale)
	return d, nil
}

func (d *BaselineDiff) add(c CellDiff) {
	// The baseline cell scored as if observed with its checksums intact.
	base := score(Attempt{Observed: c.Baseline, ChecksumsIntact: true})
	got := score(c.Observed)
	switch {
	case got < base:
		d.Regressions = append(d.Regressions, c)
	case got > base:
		d.Improvements = append(d.Improvements, c)
	case c.Observed.Observed != c.Baseline:
		d.Changes = append(d.Changes, c)
	}
}

// BaselineFromReport makes a baseline from a reference report: a cgo build
// (the parser designs measured) on ReferenceMajor. keep are the existing
// pg_overrides, kept as they are; each must still name a reported cell.
func BaselineFromReport(r Report, keep []PGOverride) (Baseline, error) {
	if !r.SQLParser {
		return Baseline{}, errors.New("the report comes from a build without the libpg_query " +
			"parser (CGO_ENABLED=0); use a cgo build's report, such as the CI artifact")
	}
	major, err := ServerMajor(r.ServerVersion)
	if err != nil {
		return Baseline{}, err
	}
	if major != ReferenceMajor {
		return Baseline{}, fmt.Errorf("the report is from PostgreSQL %d; baseline cells come "+
			"from the PG%d reference run, and a per-major difference is a pg_overrides entry "+
			"with a reason", major, ReferenceMajor)
	}
	b := Baseline{SchemaRevision: schemaRevision,
		Designs: map[string]map[string]RefusalClass{}, PGOverrides: keep}
	for _, c := range r.ReadOnly {
		for _, a := range c.Attempts {
			if b.Designs[a.Design] == nil {
				b.Designs[a.Design] = map[string]RefusalClass{}
			}
			b.Designs[a.Design][c.ID] = a.Observed
		}
	}
	if len(b.Designs) == 0 {
		return Baseline{}, errors.New("the report has no read-only cells")
	}
	if err := b.validate(); err != nil {
		return Baseline{}, fmt.Errorf("existing %w; remove or fix that entry", err)
	}
	return b, nil
}

// ReadReport decodes a bench report JSON file.
func ReadReport(path string) (Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("read report %s: %w", path, err)
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return Report{}, fmt.Errorf("parse report %s: %w", path, err)
	}
	return r, nil
}

// WriteBaseline writes b as indented JSON (map keys sorted) to path.
func WriteBaseline(path string, b Baseline) error {
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("encode baseline: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("baseline directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("write baseline %s: %w", path, err)
	}
	return nil
}
