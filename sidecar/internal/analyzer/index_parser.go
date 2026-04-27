package analyzer

import (
	"regexp"
	"strings"
)

// ParsedIndex holds the decomposed parts of a pg_get_indexdef() output.
type ParsedIndex struct {
	Schema      string
	Table       string
	Name        string
	Columns     []string
	IncludeCols []string
	WhereClause string
	IndexType   string
}

var indexDefRe = regexp.MustCompile(
	`(?i)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(\S+)\s+ON\s+(?:ONLY\s+)?` +
		`(\S+)\s+USING\s+(\w+)\s+\((.+)\)` +
		`(?:\s+INCLUDE\s+\((.+)\))?` +
		`(?:\s+WHERE\s+(.+))?$`,
)

// ParseIndexDef parses a pg_get_indexdef() string into structured parts.
func ParseIndexDef(indexdef string) ParsedIndex {
	m := indexDefRe.FindStringSubmatch(strings.TrimSpace(indexdef))
	if m == nil {
		return ParsedIndex{}
	}

	p := ParsedIndex{
		Name:      m[1],
		IndexType: strings.ToLower(m[3]),
	}

	// Table may be schema-qualified.
	table := m[2]
	if dot := strings.LastIndex(table, "."); dot >= 0 {
		p.Schema = table[:dot]
		p.Table = table[dot+1:]
	} else {
		p.Table = table
	}

	p.Columns = splitColumns(m[4])

	if m[5] != "" {
		p.IncludeCols = splitColumns(m[5])
	}
	if m[6] != "" {
		p.WhereClause = strings.TrimSpace(m[6])
	}

	return p
}

// splitColumns splits a comma-separated column list, respecting
// parenthesized expressions (e.g. "lower(name), id").
func splitColumns(s string) []string {
	var cols []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				cols = append(cols, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	cols = append(cols, strings.TrimSpace(s[start:]))
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
	return true
}
