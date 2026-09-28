package optimizer

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/pg-sage/sidecar/internal/sanitize"
)

// IndexSpec is the parsed shape of a CREATE INDEX statement. The
// optimizer derives rollback SQL and finding identity from it instead of
// trusting LLM-authored text (G3-B05, C05).
type IndexSpec struct {
	Unique      bool
	Name        string // normalized: unquoted names are case-folded
	TableSchema string // "" when the DDL is unqualified
	TableName   string
	Method      string // lowercase; btree when omitted
	Keys        string // normalized key list
	Include     string // normalized INCLUDE list
	Where       string // normalized partial-index predicate
}

// OptimizerCategory is the fixed finding category for every optimizer
// recommendation; the LLM's own label is kept as IndexCategory.
const OptimizerCategory = "missing_index"

var errNotCreateIndex = errors.New("not a CREATE INDEX statement")

// ParseIndexDDL parses CREATE [UNIQUE] INDEX [CONCURRENTLY]
// [IF NOT EXISTS] [name] ON [ONLY] table [USING m] (keys) [INCLUDE (..)]
// [NULLS [NOT] DISTINCT] [WITH (..)] [TABLESPACE t] [WHERE pred].
func ParseIndexDDL(ddl string) (IndexSpec, error) {
	s := &ddlScanner{src: strings.TrimRight(strings.TrimSpace(ddl), "; \t\n")}
	var spec IndexSpec
	if !s.keyword("CREATE") {
		return spec, errNotCreateIndex
	}
	spec.Unique = s.keyword("UNIQUE")
	if !s.keyword("INDEX") {
		return spec, errNotCreateIndex
	}
	s.keyword("CONCURRENTLY")
	if s.keyword("IF") && (!s.keyword("NOT") || !s.keyword("EXISTS")) {
		return spec, fmt.Errorf("malformed IF NOT EXISTS")
	}
	if !s.peekKeyword("ON") {
		name, err := s.identifier()
		if err != nil {
			return spec, fmt.Errorf("index name: %w", err)
		}
		spec.Name = name
	}
	if !s.keyword("ON") {
		return spec, fmt.Errorf("missing ON <table>")
	}
	s.keyword("ONLY")
	if err := s.parseTable(&spec); err != nil {
		return spec, err
	}
	return spec, s.parseBody(&spec)
}

func (s *ddlScanner) parseTable(spec *IndexSpec) error {
	first, err := s.identifier()
	if err != nil {
		return fmt.Errorf("table: %w", err)
	}
	spec.TableName = first
	if s.consume('.') {
		second, err := s.identifier()
		if err != nil {
			return fmt.Errorf("table: %w", err)
		}
		spec.TableSchema, spec.TableName = first, second
	}
	return nil
}

func (s *ddlScanner) parseBody(spec *IndexSpec) error {
	spec.Method = "btree"
	if s.keyword("USING") {
		m, err := s.identifier()
		if err != nil {
			return fmt.Errorf("index method: %w", err)
		}
		spec.Method = strings.ToLower(m)
	}
	keys, err := s.parens()
	if err != nil {
		return fmt.Errorf("key list: %w", err)
	}
	spec.Keys = normalizeFragment(keys)
	if s.keyword("INCLUDE") {
		inc, err := s.parens()
		if err != nil {
			return fmt.Errorf("include list: %w", err)
		}
		spec.Include = normalizeFragment(inc)
	}
	return s.parseTail(spec)
}

func (s *ddlScanner) parseTail(spec *IndexSpec) error {
	if s.keyword("NULLS") {
		s.keyword("NOT")
		if !s.keyword("DISTINCT") {
			return fmt.Errorf("malformed NULLS DISTINCT")
		}
	}
	if s.keyword("WITH") {
		if _, err := s.parens(); err != nil {
			return fmt.Errorf("storage parameters: %w", err)
		}
	}
	if s.keyword("TABLESPACE") {
		if _, err := s.identifier(); err != nil {
			return fmt.Errorf("tablespace: %w", err)
		}
	}
	if s.keyword("WHERE") {
		spec.Where = normalizeFragment(s.rest())
		if spec.Where == "" {
			return fmt.Errorf("empty WHERE predicate")
		}
	}
	if strings.TrimSpace(s.rest()) != "" {
		return fmt.Errorf("unexpected trailing text %q", s.rest())
	}
	return nil
}

// Fingerprint is the normalized index definition, independent of the
// index name and formatting.
func (spec IndexSpec) Fingerprint() string {
	var b strings.Builder
	b.WriteString(spec.Method + "(" + spec.Keys + ")")
	if spec.Include != "" {
		b.WriteString(" include(" + spec.Include + ")")
	}
	if spec.Where != "" {
		b.WriteString(" where " + spec.Where)
	}
	return b.String()
}

// FindingIdentifier is the finding identity for a recommendation:
// "schema.table|<index fingerprint>" (C05, G2-B19, G3-B13). A DDL that
// cannot be parsed falls back to the bare table (legacy identity).
func (r Recommendation) FindingIdentifier() string {
	spec, err := ParseIndexDDL(r.DDL)
	if err != nil || r.Table == "" {
		return r.Table
	}
	return r.Table + "|" + spec.Fingerprint()
}

// canonicalizeRecommendation binds an LLM recommendation to the analyzed
// table: the DDL must be a named, non-UNIQUE CREATE INDEX on tc; Table,
// DropDDL and Category are derived deterministically, and the LLM's
// self-rated risk is discarded (G3-B05, G3-B20, G3-B24).
func canonicalizeRecommendation(rec Recommendation, tc TableContext) (Recommendation, error) {
	spec, err := ParseIndexDDL(rec.DDL)
	if err != nil {
		return rec, fmt.Errorf("unsupported DDL: %w", err)
	}
	if spec.Unique {
		return rec, fmt.Errorf("UNIQUE indexes change semantics; not a performance recommendation")
	}
	if spec.Name == "" {
		return rec, fmt.Errorf("index name required for a deterministic rollback")
	}
	if !targetsTable(spec, tc) {
		return rec, fmt.Errorf("DDL targets %s.%s, not the analyzed table %s.%s",
			spec.TableSchema, spec.TableName, tc.Schema, tc.Table)
	}
	rec.Table = tc.Schema + "." + tc.Table
	rec.DropDDL = "DROP INDEX CONCURRENTLY IF EXISTS " +
		sanitize.QuoteQualifiedName(tc.Schema, spec.Name)
	if rec.IndexCategory == "" {
		rec.IndexCategory = rec.Category
	}
	rec.Category = OptimizerCategory
	rec.ActionRisk = ""
	return rec, nil
}

func targetsTable(spec IndexSpec, tc TableContext) bool {
	if spec.TableName != tc.Table {
		return false
	}
	if spec.TableSchema == "" {
		return tc.Schema == "public"
	}
	return spec.TableSchema == tc.Schema
}

// normalizeFragment collapses whitespace and case-folds everything
// outside quoted identifiers and string literals.
func normalizeFragment(frag string) string {
	var b strings.Builder
	var quote rune
	space := false
	for _, r := range strings.TrimSpace(frag) {
		switch {
		case quote != 0:
			b.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		case r == '"' || r == '\'':
			quote = r
		case unicode.IsSpace(r):
			space = true
			continue
		}
		if space && !isTight(r) && !endsTight(b.String()) {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func isTight(r rune) bool { return r == '(' || r == ')' || r == ',' }

func endsTight(s string) bool {
	return s == "" || strings.HasSuffix(s, "(") || strings.HasSuffix(s, ",")
}
