package safetybench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// BaselineFile is the committed baseline, relative to this package.
const BaselineFile = "testdata/baseline.json"

// RegenerateCommand rewrites BaselineFile from a bench report (run from
// sidecar/).
const RegenerateCommand = "go run ./safety-bench/cmd/baseline"

// ReferenceMajor is the PostgreSQL major whose report the baseline cells
// come from (the signed, shipped run). Other majors differ only through
// pg_overrides entries, each with a reason.
const ReferenceMajor = 17

// parserDesigns need the libpg_query parser (a cgo build). Without it they
// refuse every statement, so their cells say nothing about the shipped
// guard and are not compared against the baseline.
var parserDesigns = map[string]bool{"explain_guard": true, "agent_query": true}

// Baseline is the observed class of every (design, case) cell on the
// reference major, plus per-major differences.
type Baseline struct {
	// SchemaRevision is the report schema revision the cells were written
	// with. Absent (0) means revision 1, before leaked_in_error existed.
	SchemaRevision int `json:"schema_revision,omitempty"`
	// Designs maps design -> case id -> observed class.
	Designs map[string]map[string]RefusalClass `json:"designs"`
	// PGOverrides replace a cell on one major. Each needs a reason.
	PGOverrides []PGOverride `json:"pg_overrides,omitempty"`
}

// PGOverride is a cell whose observed class differs on one major.
type PGOverride struct {
	Major    int          `json:"major"`
	Design   string       `json:"design"`
	Case     string       `json:"case"`
	Observed RefusalClass `json:"observed"`
	Reason   string       `json:"reason"`
}

// LoadBaseline reads and validates the baseline at path. Every error names
// the path.
func LoadBaseline(path string) (Baseline, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Baseline{}, fmt.Errorf("read baseline %s: %w", path, err)
	}
	b, err := ParseBaseline(raw)
	if err != nil {
		return Baseline{}, fmt.Errorf("baseline %s: %w", path, err)
	}
	return b, nil
}

// ParseBaseline decodes and validates a baseline. An empty document is an
// empty baseline: every observed cell then has no baseline row.
func ParseBaseline(raw []byte) (Baseline, error) {
	var b Baseline
	if len(bytes.TrimSpace(raw)) == 0 {
		return b, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return Baseline{}, fmt.Errorf("parse JSON: %w", err)
	}
	if dec.More() {
		return Baseline{}, errors.New("parse JSON: trailing data after the baseline object")
	}
	return b, b.validate()
}

// ParserRequired reports whether this run must have the libpg_query
// parser. CI (GITHUB_ACTIONS=true) always builds with cgo; a parser-less
// run there would publish explain_guard and agent_query as holding every
// case while the baseline comparison skips their cells.
func ParserRequired(getenv func(string) string) bool {
	return getenv("GITHUB_ACTIONS") == "true"
}

func (b Baseline) validate() error {
	if b.SchemaRevision < 0 || b.SchemaRevision > schemaRevision {
		return fmt.Errorf("schema_revision %d is not one this build reads (1..%d)",
			b.SchemaRevision, schemaRevision)
	}
	for design, cells := range b.Designs {
		for id, class := range cells {
			if !knownClass(class) {
				return fmt.Errorf("cell %s %s: unknown class %q", design, id, class)
			}
		}
	}
	seen := map[string]bool{}
	for i, o := range b.PGOverrides {
		where := fmt.Sprintf("pg_overrides[%d] (%s %s PG%d)", i, o.Design, o.Case, o.Major)
		key := fmt.Sprintf("%d/%s/%s", o.Major, o.Design, o.Case)
		switch {
		case strings.TrimSpace(o.Reason) == "":
			return fmt.Errorf("%s: reason is required", where)
		case o.Major <= 0:
			return fmt.Errorf("%s: major must be a PostgreSQL major version", where)
		case o.Major == ReferenceMajor:
			return fmt.Errorf("%s: PG%d is the reference major; its cells are the base "+
				"cells, so regenerate them instead", where, ReferenceMajor)
		case !knownClass(o.Observed):
			return fmt.Errorf("%s: unknown class %q", where, o.Observed)
		case seen[key]:
			return fmt.Errorf("%s: duplicate override", where)
		}
		if _, ok := b.Designs[o.Design][o.Case]; !ok {
			return fmt.Errorf("%s: overrides a cell the baseline does not have", where)
		}
		seen[key] = true
	}
	return nil
}

func knownClass(c RefusalClass) bool {
	return c.Credited() || c == ClassOtherError || c == ClassExecuted ||
		c == ClassLeakedInError
}

// cellClass is the baseline class of a cell, read in the current schema. A
// revision-1 baseline predates leaked_in_error: it recorded a leak on a
// sentinel case as other_error, so that cell reads as leaked_in_error.
func (b Baseline) cellClass(class RefusalClass, sentinelCase bool) RefusalClass {
	if b.SchemaRevision < 2 && sentinelCase && class == ClassOtherError {
		return ClassLeakedInError
	}
	return class
}

// cellsFor returns the expected cells on major: the base cells with that
// major's overrides applied.
func (b Baseline) cellsFor(major int) map[string]map[string]RefusalClass {
	out := make(map[string]map[string]RefusalClass, len(b.Designs))
	for design, cells := range b.Designs {
		out[design] = make(map[string]RefusalClass, len(cells))
		for id, class := range cells {
			out[design][id] = class
		}
	}
	for _, o := range b.PGOverrides {
		if o.Major == major {
			out[o.Design][o.Case] = o.Observed
		}
	}
	return out
}

var majorRE = regexp.MustCompile(`^PostgreSQL (\d+)`)

// ServerMajor reads the major version from a version() string.
func ServerMajor(version string) (int, error) {
	m := majorRE.FindStringSubmatch(strings.TrimSpace(version))
	if m == nil {
		return 0, fmt.Errorf("cannot read the PostgreSQL major from server version %q", version)
	}
	return strconv.Atoi(m[1])
}
