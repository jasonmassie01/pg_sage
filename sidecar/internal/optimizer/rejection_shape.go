package optimizer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// candidateShape is the identity of an index idea for rejection memory: the
// access method, the ordered keys with their opclass, collation and
// non-default ordering, the partial-index predicate, and the INCLUDE
// columns as a set. The index name and the table qualification are not
// part of it (the memory is keyed by table separately).
type candidateShape struct {
	Method    string
	Keys      []string
	Predicate string
	Include   []string // sorted and deduplicated; nil when empty
}

var errEmptyIndexKey = errors.New("empty index key")

// shapeOfCandidate parses a CREATE INDEX statement into its normalized
// shape. A statement it cannot normalize is an error: such a candidate is
// never matched against memory (it is measured instead).
func shapeOfCandidate(ddl string) (candidateShape, error) {
	spec, err := ParseIndexDDL(ddl)
	if err != nil {
		return candidateShape{}, err
	}
	parts, err := splitTopLevel(spec.Keys)
	if err != nil {
		return candidateShape{}, err
	}
	keys := make([]string, 0, len(parts))
	for _, part := range parts {
		key, err := canonicalKey(part)
		if err != nil {
			return candidateShape{}, fmt.Errorf("index key %q: %w", part, err)
		}
		keys = append(keys, key)
	}
	include, err := canonicalIncludeSet(spec.Include)
	if err != nil {
		return candidateShape{}, err
	}
	return candidateShape{Method: spec.Method, Keys: keys,
		Predicate: canonicalPredicate(spec.Where), Include: include}, nil
}

// sameIdea reports whether two shapes are one index idea: same method,
// keys and predicate, and INCLUDE sets of which one contains the other.
func (s candidateShape) sameIdea(o candidateShape) bool {
	if s.Method == "" || len(s.Keys) == 0 || s.Method != o.Method ||
		s.Predicate != o.Predicate || !slices.Equal(s.Keys, o.Keys) {
		return false
	}
	return isSubset(s.Include, o.Include) || isSubset(o.Include, s.Include)
}

func isSubset(small, big []string) bool {
	for _, c := range small {
		if !slices.Contains(big, c) {
			return false
		}
	}
	return true
}

// hash identifies the exact shape (INCLUDE order does not matter, the
// INCLUDE set does): the hex SHA-256 of its JSON encoding, so list items
// can never collide through a separator.
func (s candidateShape) hash() string {
	raw, err := json.Marshal(struct {
		M string   `json:"m"`
		K []string `json:"k"`
		P string   `json:"p"`
		I []string `json:"i"`
	}{s.Method, s.Keys, s.Predicate, s.Include})
	if err != nil { // unreachable: strings and string slices always encode
		raw = []byte(fmt.Sprint(s))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// String renders the shape for the optimizer prompt and the logs.
func (s candidateShape) String() string {
	out := s.Method + " (" + strings.Join(s.Keys, ", ") + ")"
	if len(s.Include) > 0 {
		out += " INCLUDE (" + strings.Join(s.Include, ", ") + ")"
	}
	if s.Predicate != "" {
		out += " WHERE " + s.Predicate
	}
	return out
}

// canonicalKey normalizes one entry of a (normalizeFragment'ed) key list:
// expression, then COLLATE, opclass (with parameters), ASC/DESC and NULLS
// in any order. Default ordering (ASC, NULLS LAST for ASC, NULLS FIRST for
// DESC) is dropped.
func canonicalKey(part string) (string, error) {
	s := &ddlScanner{src: part}
	expr, err := s.keyExpression()
	if err != nil {
		return "", err
	}
	d, err := s.keyDecorations()
	if err != nil {
		return "", err
	}
	out := stripOuterParens(canonicalSQL(expr))
	if d.collation != "" {
		out += " collate " + d.collation
	}
	if d.opclass != "" {
		out += " " + d.opclass
	}
	if d.order == "desc" {
		out += " desc"
	}
	defaultNulls := "last"
	if d.order == "desc" {
		defaultNulls = "first"
	}
	if d.nulls != "" && d.nulls != defaultNulls {
		out += " nulls " + d.nulls
	}
	return out, nil
}

// keyExpression reads a key's expression: a parenthesized expression, or a
// (qualified) name optionally followed by a call's argument list.
func (s *ddlScanner) keyExpression() (string, error) {
	s.skipSpace()
	start := s.pos
	if s.pos >= len(s.src) {
		return "", errEmptyIndexKey
	}
	if s.src[s.pos] != '(' {
		if _, err := s.qualifiedName(); err != nil {
			return "", err
		}
	}
	if s.peek('(') {
		if _, err := s.parens(); err != nil {
			return "", err
		}
	}
	return s.src[start:s.pos], nil
}

type keyDecor struct{ collation, opclass, order, nulls string }

func (s *ddlScanner) keyDecorations() (keyDecor, error) {
	var d keyDecor
	for s.rest() != "" {
		switch {
		case s.keyword("collate"):
			name, err := s.qualifiedName()
			if err != nil {
				return d, fmt.Errorf("collation: %w", err)
			}
			d.collation = name
		case s.keyword("asc"):
			d.order = "asc"
		case s.keyword("desc"):
			d.order = "desc"
		case s.keyword("nulls"):
			switch {
			case s.keyword("first"):
				d.nulls = "first"
			case s.keyword("last"):
				d.nulls = "last"
			default:
				return d, fmt.Errorf("malformed NULLS ordering at %q", s.rest())
			}
		default:
			if err := s.opclass(&d); err != nil {
				return d, err
			}
		}
	}
	return d, nil
}

// opclass reads an operator class and its optional parameter list.
func (s *ddlScanner) opclass(d *keyDecor) error {
	if d.opclass != "" {
		return fmt.Errorf("unexpected %q after operator class", s.rest())
	}
	name, err := s.qualifiedName()
	if err != nil {
		return fmt.Errorf("operator class: %w", err)
	}
	start := s.pos
	if s.peek('(') {
		if _, err := s.parens(); err != nil {
			return fmt.Errorf("operator class parameters: %w", err)
		}
	}
	d.opclass = name + canonicalSQL(s.src[start:s.pos])
	return nil
}

// qualifiedName reads a dotted name and returns it canonically quoted,
// without a pg_catalog qualifier.
func (s *ddlScanner) qualifiedName() (string, error) {
	var parts []string
	for {
		id, err := s.identifier()
		if err != nil {
			return "", err
		}
		parts = append(parts, canonicalIdent(id))
		if !s.consume('.') {
			break
		}
	}
	if len(parts) > 1 && parts[0] == "pg_catalog" {
		parts = parts[1:]
	}
	return strings.Join(parts, "."), nil
}

func (s *ddlScanner) peek(c byte) bool {
	s.skipSpace()
	return s.pos < len(s.src) && s.src[s.pos] == c
}

var plainLowerIdent = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)

// shapeKeywords stay quoted as identifiers: bare, they would read as the
// grammar words the shape normalizer splits on.
var shapeKeywords = map[string]bool{
	"and": true, "or": true, "not": true, "between": true, "collate": true, "asc": true,
	"desc": true, "nulls": true, "first": true, "last": true, "is": true, "null": true,
	"in": true, "where": true, "include": true, "using": true, "on": true, "default": true,
}

// canonicalIdent spells an identifier the way it must be written: bare when
// PostgreSQL would fold it to itself, double-quoted otherwise.
func canonicalIdent(id string) string {
	if plainLowerIdent.MatchString(id) && !shapeKeywords[id] {
		return id
	}
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

// canonicalIncludeSet normalizes an INCLUDE list into a sorted set.
func canonicalIncludeSet(list string) ([]string, error) {
	if strings.TrimSpace(list) == "" {
		return nil, nil
	}
	parts, err := splitTopLevel(list)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, part := range parts {
		col := canonicalSQL(part)
		if col == "" {
			return nil, fmt.Errorf("empty INCLUDE column in %q", list)
		}
		if !slices.Contains(out, col) {
			out = append(out, col)
		}
	}
	sort.Strings(out)
	return out, nil
}
