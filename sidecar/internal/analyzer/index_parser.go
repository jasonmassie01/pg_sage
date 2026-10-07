package analyzer

import (
	"regexp"
	"slices"
	"strings"
	"sync"
)

// ParsedIndex holds the decomposed parts of a pg_get_indexdef() string.
// Schema, Table and simple column names are unquoted ("Sales" -> Sales)
// so they compare equal to catalog names.
type ParsedIndex struct {
	Schema      string
	Table       string
	Name        string
	Columns     []string
	IncludeCols []string
	WhereClause string
	IndexType   string
}

// indexDefHead matches everything up to the opening parenthesis of the
// key column list. The key list, INCLUDE list and WHERE predicate are
// then extracted with a paren/quote-aware scanner: the previous greedy
// regex swallowed INCLUDE and WHERE into the key columns (G2-B13).
var indexDefHead = regexp.MustCompile(
	`(?i)^CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?` +
		`(?:IF\s+NOT\s+EXISTS\s+)?(\S+)\s+ON\s+(?:ONLY\s+)?` +
		`(\S+)\s+USING\s+(\w+)\s*\(`,
)

// indexDefCacheMax bounds the memoized parses: a catalog's definitions
// repeat every cycle, so the cache holds them all on any real database and
// is emptied, not grown, past the cap.
const indexDefCacheMax = 100_000

var indexDefCache = struct {
	sync.Mutex
	m map[string]ParsedIndex
}{m: map[string]ParsedIndex{}}

// ParseIndexDef parses a pg_get_indexdef() string into structured parts.
// Parses are memoized (the same definitions come back every cycle); each
// call returns its own copy of the column slices.
func ParseIndexDef(indexdef string) ParsedIndex {
	indexDefCache.Lock()
	p, ok := indexDefCache.m[indexdef]
	indexDefCache.Unlock()
	if !ok {
		p = parseIndexDef(indexdef)
		indexDefCache.Lock()
		if len(indexDefCache.m) >= indexDefCacheMax {
			indexDefCache.m = map[string]ParsedIndex{}
		}
		indexDefCache.m[indexdef] = p
		indexDefCache.Unlock()
	}
	p.Columns = slices.Clone(p.Columns)
	p.IncludeCols = slices.Clone(p.IncludeCols)
	return p
}

// indexDefCacheLen is the number of memoized parses (tests).
func indexDefCacheLen() int {
	indexDefCache.Lock()
	defer indexDefCache.Unlock()
	return len(indexDefCache.m)
}

func parseIndexDef(indexdef string) ParsedIndex {
	def := strings.TrimSpace(indexdef)
	loc := indexDefHead.FindStringSubmatchIndex(def)
	if loc == nil {
		return ParsedIndex{}
	}
	m := func(i int) string { return def[loc[2*i]:loc[2*i+1]] }
	p := ParsedIndex{Name: unquoteIdent(m(1)), IndexType: strings.ToLower(m(3))}
	p.Schema, p.Table = splitQualified(m(2))

	keys, rest, ok := parenGroup(def[loc[1]-1:])
	if !ok {
		return ParsedIndex{}
	}
	p.Columns = unquoteColumns(splitColumns(keys))
	rest = strings.TrimSpace(rest)
	if upper := strings.ToUpper(rest); strings.HasPrefix(upper, "INCLUDE") {
		inc, after, ok := parenGroup(strings.TrimSpace(rest[len("INCLUDE"):]))
		if !ok {
			return ParsedIndex{}
		}
		p.IncludeCols = unquoteColumns(splitColumns(inc))
		rest = after
	}
	p.WhereClause = topLevelWhere(rest)
	return p
}

// parenGroup expects s to start with "(" and returns the text inside the
// matching ")" and the text after it. Quotes and literals are respected.
func parenGroup(s string) (inside, rest string, ok bool) {
	if !strings.HasPrefix(s, "(") {
		return "", "", false
	}
	depth := 0
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
			if depth == 0 {
				return s[1:i], s[i+1:], true
			}
		}
	}
	return "", "", false
}

// topLevelWhere returns the predicate after a WHERE keyword that is
// outside parentheses and quotes, or "".
func topLevelWhere(s string) string {
	s = " " + s
	upper := strings.ToUpper(s)
	depth := 0
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
		case depth == 0 && strings.HasPrefix(upper[i:], " WHERE "):
			return strings.TrimSpace(s[i+len(" WHERE "):])
		}
	}
	return ""
}

// splitColumns splits a comma-separated column list, respecting
// parenthesized expressions (e.g. "lower(name), id") and quotes.
func splitColumns(s string) []string {
	var cols []string
	depth := 0
	start := 0
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
			cols = append(cols, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(cols, strings.TrimSpace(s[start:]))
}

// splitQualified splits a possibly quoted "schema.table" at the dot that
// is outside quotes and unquotes both parts.
func splitQualified(name string) (schema, table string) {
	inQuote := false
	for i := len(name) - 1; i >= 0; i-- {
		switch name[i] {
		case '"':
			inQuote = !inQuote
		case '.':
			if !inQuote {
				return unquoteIdent(name[:i]), unquoteIdent(name[i+1:])
			}
		}
	}
	return "", unquoteIdent(name)
}

// unquoteIdent strips identifier quotes: "Foo""Bar" -> Foo"Bar.
func unquoteIdent(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

// unquoteColumns unquotes columns that are a single quoted identifier,
// leaving expressions untouched.
func unquoteColumns(cols []string) []string {
	for i, c := range cols {
		inner := c
		if len(c) >= 2 && c[0] == '"' && c[len(c)-1] == '"' {
			inner = strings.ReplaceAll(c[1:len(c)-1], `""`, "")
		}
		if !strings.Contains(inner, `"`) {
			cols[i] = unquoteIdent(c)
		}
	}
	return cols
}

// IsDuplicate returns true if a and b cover the exact same columns
// in the same order, on the same table, with the same WHERE clause
// and INCLUDE columns.
func IsDuplicate(a, b ParsedIndex) bool {
	if a.Schema != b.Schema ||
		a.Table != b.Table ||
		a.WhereClause != b.WhereClause {
		return false
	}
	if len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		if a.Columns[i] != b.Columns[i] {
			return false
		}
	}
	if len(a.IncludeCols) != len(b.IncludeCols) {
		return false
	}
	for i := range a.IncludeCols {
		if a.IncludeCols[i] != b.IncludeCols[i] {
			return false
		}
	}
	return true
}

// IsSubset returns true if a's columns are a leading prefix of b's columns,
// on the same table with the same WHERE clause.
func IsSubset(a, b ParsedIndex) bool {
	if a.Schema != b.Schema ||
		a.Table != b.Table ||
		a.WhereClause != b.WhereClause {
		return false
	}
	if len(a.Columns) >= len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		if a.Columns[i] != b.Columns[i] {
			return false
		}
	}
	// a's INCLUDE columns must also be served by b (as a key or INCLUDE
	// column); otherwise a supports index-only scans that b does not, and
	// dropping a would regress those queries.
	bServes := make(map[string]bool, len(b.Columns)+len(b.IncludeCols))
	for _, c := range b.Columns {
		bServes[c] = true
	}
	for _, c := range b.IncludeCols {
		bServes[c] = true
	}
	for _, c := range a.IncludeCols {
		if !bServes[c] {
			return false
		}
	}
	return true
}
