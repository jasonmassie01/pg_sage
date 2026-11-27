package optimizer

import (
	"fmt"
	"regexp"
	"strings"
)

// indexKey is one entry of an index key or INCLUDE list: a plain column,
// or an expression with the identifiers it references, and the collation,
// operator class and ordering the key was declared with.
type indexKey struct {
	column string   // plain column (quotes removed); "" for an expression
	expr   string   // normalized expression; "" for a plain column
	refs   []string // identifiers an expression references
	decor  keyDecor // as written; "" fields were not given
}

// parseIndexKeys parses the key list of a CREATE INDEX statement (or a
// pg_get_indexdef definition) with the balanced-paren DDL scanner, so
// expression keys such as (lower(email)) or ((payload->>'k')) are kept
// whole instead of being cut at their first ')' (Phase 0 item 7).
func parseIndexKeys(ddl string) ([]indexKey, error) {
	spec, err := ParseIndexDDL(ddl)
	if err != nil {
		return nil, err
	}
	return splitKeyList(spec.Keys)
}

// splitKeyList splits a normalized key or INCLUDE list at top-level
// commas into keys, keeping each key's COLLATE, operator class (with its
// parameters), ASC/DESC and NULLS ordering. A decoration the scanner
// cannot read is an error, so a malformed key never matches anything.
func splitKeyList(list string) ([]indexKey, error) {
	parts, err := splitTopLevel(list)
	if err != nil {
		return nil, err
	}
	keys := make([]indexKey, 0, len(parts))
	for _, part := range parts {
		key, err := parseKey(part)
		if err != nil {
			return nil, fmt.Errorf("index key %q in %q: %w", part, list, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// parseKey reads one key: its expression with the DDL scanner, then its
// decorations in any order.
func parseKey(part string) (indexKey, error) {
	s := &ddlScanner{src: part}
	expr, err := s.keyExpression()
	if err != nil {
		return indexKey{}, err
	}
	d, err := s.keyDecorations()
	if err != nil {
		return indexKey{}, err
	}
	key := keyOf(strings.TrimSpace(expr))
	if key.column == "" && key.expr == "" {
		return indexKey{}, errEmptyIndexKey
	}
	key.decor = d
	return key, nil
}

// splitTopLevel splits s at commas outside parentheses, quoted
// identifiers and string literals.
func splitTopLevel(s string) ([]string, error) {
	spans, err := topLevelSpans(s)
	if err != nil {
		return nil, err
	}
	parts := make([]string, len(spans))
	for i, sp := range spans {
		parts[i] = strings.TrimSpace(s[sp[0]:sp[1]])
	}
	return parts, nil
}

// topLevelSpans returns the [start, end) spans of s between the commas
// outside parentheses, quoted identifiers and string literals.
func topLevelSpans(s string) ([][2]int, error) {
	var spans [][2]int
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			spans = append(spans, [2]int{start, i})
			start = i + 1
		}
	}
	if depth != 0 || quote != 0 {
		return nil, fmt.Errorf("unbalanced key list %q", s)
	}
	return append(spans, [2]int{start, len(s)}), nil
}

var plainIdent = regexp.MustCompile(`^(?:[a-z_][a-z0-9_$]*|"(?:[^"]|"")+")$`)

func keyOf(key string) indexKey {
	if plainIdent.MatchString(key) {
		return indexKey{column: unquoteIdent(key)}
	}
	if key == "" {
		return indexKey{}
	}
	return indexKey{expr: key, refs: expressionRefs(key)}
}

func unquoteIdent(id string) string {
	if strings.HasPrefix(id, `"`) && strings.HasSuffix(id, `"`) && len(id) >= 2 {
		return strings.ReplaceAll(id[1:len(id)-1], `""`, `"`)
	}
	return id
}

// exprKeywords are SQL words that can appear bare inside an index
// expression without naming a column.
var exprKeywords = map[string]bool{
	"and": true, "or": true, "not": true, "null": true, "true": true, "false": true,
	"is": true, "in": true, "like": true, "ilike": true, "similar": true, "to": true,
	"between": true, "case": true, "when": true, "then": true, "else": true, "end": true,
	"distinct": true, "from": true, "as": true, "at": true, "time": true, "zone": true,
	"interval": true, "collate": true, "any": true, "all": true, "some": true,
	"array": true, "escape": true, "varying": true, "precision": true, "with": true,
	"without": true, "isnull": true, "notnull": true, "year": true, "month": true,
	"day": true, "hour": true, "minute": true, "second": true, "epoch": true,
	"dow": true, "doy": true, "week": true, "quarter": true,
}

// expressionRefs returns the identifiers an index expression references:
// quoted identifiers, and bare words that are not function names, cast
// types, keywords or inside string literals. Order is kept, duplicates
// dropped.
func expressionRefs(expr string) []string {
	var refs []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			refs = append(refs, id)
		}
	}
	for i := 0; i < len(expr); {
		c := expr[i]
		switch {
		case c == '\'':
			i = skipQuoted(expr, i, '\'')
		case c == '"':
			end := skipQuoted(expr, i, '"')
			add(unquoteIdent(expr[i:end]))
			i = end
		case isIdentStart(c):
			end := i
			for end < len(expr) && isIdentByte(expr[end]) {
				end++
			}
			if isColumnRef(expr, i, end) {
				add(expr[i:end])
			}
			i = end
		case c >= '0' && c <= '9':
			for i < len(expr) && isIdentByte(expr[i]) {
				i++
			}
		default:
			i++
		}
	}
	return refs
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// skipQuoted returns the index just past the quoted run starting at i
// (a doubled quote character is an escaped one).
func skipQuoted(s string, i int, q byte) int {
	for j := i + 1; j < len(s); j++ {
		if s[j] != q {
			continue
		}
		if j+1 < len(s) && s[j+1] == q {
			j++
			continue
		}
		return j + 1
	}
	return len(s)
}

// isColumnRef reports whether the word s[start:end] names a column: it is
// not a keyword, not a function name (followed by '(') and not a cast type
// (preceded by "::").
func isColumnRef(s string, start, end int) bool {
	if exprKeywords[strings.ToLower(s[start:end])] {
		return false
	}
	if next := strings.TrimLeft(s[end:], " "); strings.HasPrefix(next, "(") {
		return false
	}
	prev := strings.TrimRight(s[:start], " ")
	return !strings.HasSuffix(prev, "::")
}
