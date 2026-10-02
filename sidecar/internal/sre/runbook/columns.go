package runbook

import (
	"regexp"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The columns a probe returns, read from its fixed SQL: the names of the
// main (first top-level) SELECT list. Comments and string literals are
// removed first, parenthesized subqueries and CTE bodies are skipped, and
// each item is named the way PostgreSQL names it (its alias, else the
// column or function name). columns_db_test.go checks the result against
// every catalog probe on a real server.

var (
	aliasPattern = regexp.MustCompile(`(?is)\bas\s+"?([a-z_][a-z0-9_]*)"?\s*$`)
	castPattern  = regexp.MustCompile(`(?is)(::\s*[a-z_][a-z0-9_. ]*(\[\])?\s*)+$`)
	identPattern = regexp.MustCompile(`(?i)"?([a-z_][a-z0-9_]*)"?\s*$`)
	columnName   = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

// listEnd are the keywords that end a top-level SELECT list.
var listEnd = []string{"from", "union", "intersect", "except", "where", "group", "order",
	"limit", "having", "window", "into", "offset", "fetch", "for"}

// ColumnsOf returns the result column names of a SQL query, or nil.
func ColumnsOf(sql string) []string {
	clean := strings.ToLower(stripCommentsAndLiterals(sql))
	start := keywordAt(clean, 0, "select")
	if start < 0 {
		return nil
	}
	body := clean[start+len("select"):]
	end := len(body)
	for _, kw := range listEnd {
		if i := keywordAt(body, 0, kw); i >= 0 && i < end {
			end = i
		}
	}
	list := strings.TrimSpace(body[:end])
	list = strings.TrimSpace(strings.TrimPrefix(list, "distinct"))
	var out []string
	for _, item := range splitTopLevel(list) {
		if name := itemName(item); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// stripCommentsAndLiterals drops comments and replaces each quoted string
// literal with an empty literal so neither can hide keywords, commas or
// parentheses.
func stripCommentsAndLiterals(sql string) string {
	var b strings.Builder
	for i := 0; i < len(sql); i++ {
		switch {
		case strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
			b.WriteByte(' ')
		case strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				return b.String()
			}
			i += end
			b.WriteByte('\n')
		case sql[i] == '\'':
			i = skipLiteral(sql, i)
			b.WriteString("''")
		default:
			b.WriteByte(sql[i])
		}
	}
	return b.String()
}

// skipLiteral returns the index of the quote closing the literal at i
// (a doubled quote inside a literal is an escaped quote).
func skipLiteral(sql string, i int) int {
	for j := i + 1; j < len(sql); j++ {
		if sql[j] != '\'' {
			continue
		}
		if j+1 < len(sql) && sql[j+1] == '\'' {
			j++
			continue
		}
		return j
	}
	return len(sql) - 1
}

// keywordAt returns the index of the first top-level (parenthesis depth 0)
// whole-word occurrence of kw in s at or after from, or -1.
func keywordAt(s string, from int, kw string) int {
	depth := 0
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
			continue
		case ')':
			depth--
			continue
		}
		if depth == 0 && strings.HasPrefix(s[i:], kw) && boundary(s, i-1) &&
			boundary(s, i+len(kw)) {
			return i
		}
	}
	return -1
}

func boundary(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return true
	}
	return !identByte(s[i])
}

func identByte(c byte) bool {
	return c == '_' || c == '.' || c == '"' || (c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9')
}

func splitTopLevel(list string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(list); i++ {
		switch list[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, list[start:i])
				start = i + 1
			}
		}
	}
	return append(out, list[start:])
}

// itemName is the column name PostgreSQL gives a select-list item.
func itemName(item string) string {
	item = strings.TrimSpace(item)
	if m := aliasPattern.FindStringSubmatch(item); m != nil {
		return m[1]
	}
	item = strings.TrimSpace(castPattern.ReplaceAllString(item, ""))
	if strings.HasSuffix(item, ")") {
		if open := matchingOpen(item); open > 0 {
			item = strings.TrimSpace(item[:open])
		}
	}
	if m := identPattern.FindStringSubmatch(item); m != nil {
		return m[1]
	}
	return ""
}

// matchingOpen is the index of the parenthesis that the final ')' closes.
func matchingOpen(s string) int {
	depth := 0
	for i := len(s) - 1; i >= 0; i-- {
		switch s[i] {
		case ')':
			depth++
		case '(':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// OutputColumns is the union of the columns of a probe's SQL variants, in
// first-seen order, or nil without SQL.
func OutputColumns(spec probes.Spec) []string {
	var out []string
	for _, v := range spec.Variants {
		for _, c := range ColumnsOf(v.SQL) {
			if !containsString(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
}

var (
	columnsMu    sync.Mutex
	columnsCache = map[probes.ID][]string{}
)

// HasColumn reports whether catalog probe id returns column.
func HasColumn(id probes.ID, column string) bool {
	if !columnName.MatchString(column) {
		return false
	}
	spec, ok := probes.Catalog().Spec(id)
	if !ok {
		return false
	}
	columnsMu.Lock()
	cols, cached := columnsCache[id]
	if !cached {
		cols = OutputColumns(spec)
		columnsCache[id] = cols
	}
	columnsMu.Unlock()
	return containsString(cols, column)
}
