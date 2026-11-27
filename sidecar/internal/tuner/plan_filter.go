package tuner

import (
	"regexp"
	"strings"
)

// btreeOperators are the comparison operators a btree index scan can use
// on a leading column, in the spacing EXPLAIN prints them.
var btreeOperators = []string{" = ANY ", " >= ", " <= ", " = ", " < ", " > "}

var filterIdent = regexp.MustCompile(`^(?:[a-z_][a-z0-9_$]*|"(?:[^"]|"")+")$`)

// filterColumns returns the columns that a top-level conjunct of an
// EXPLAIN Filter compares with a btree operator, e.g. "customer_id" for
// "((note = 'x'::text) AND (customer_id = 7))". A top-level OR, a
// function or expression over the column, or an unparseable filter
// yields none.
func filterColumns(filter string) map[string]bool {
	cols := map[string]bool{}
	list, _ := conjunctColumns(filter)
	for _, col := range list {
		cols[col] = true
	}
	return cols
}

// conjunctColumns is filterColumns in conjunct order without duplicates;
// equality holds the columns compared with = or = ANY.
func conjunctColumns(filter string) (cols []string, equality map[string]bool) {
	equality = map[string]bool{}
	expr := unwrapParens(strings.TrimSpace(filter))
	if expr == "" || len(topLevelSplit(expr, " OR ")) > 1 {
		return nil, equality
	}
	seen := map[string]bool{}
	for _, conjunct := range topLevelSplit(expr, " AND ") {
		col, op := comparedColumn(unwrapParens(conjunct))
		if col == "" {
			continue
		}
		if !seen[col] {
			seen[col] = true
			cols = append(cols, col)
		}
		if op == " = " || op == " = ANY " {
			equality[col] = true
		}
	}
	return cols, equality
}

// comparedColumn returns the plain column on the left of the first
// top-level btree comparison in conjunct and that operator, or "".
func comparedColumn(conjunct string) (col, op string) {
	for _, op := range btreeOperators {
		parts := topLevelSplit(conjunct, op)
		if len(parts) < 2 {
			continue
		}
		lhs := unwrapParens(stripCasts(unwrapParens(parts[0])))
		if i := strings.LastIndex(lhs, "."); i >= 0 && !strings.HasSuffix(lhs, `"`) {
			lhs = lhs[i+1:] // alias-qualified: o.customer_id
		}
		if !filterIdent.MatchString(lhs) {
			return "", ""
		}
		return strings.ReplaceAll(strings.Trim(lhs, `"`), `""`, `"`), op
	}
	return "", ""
}

var trailingCast = regexp.MustCompile(`::[a-z_ ]+(\[\])?$`)

func stripCasts(s string) string {
	for {
		next := trailingCast.ReplaceAllString(s, "")
		if next == s {
			return s
		}
		s = next
	}
}

// unwrapParens removes parentheses that enclose the whole of s.
func unwrapParens(s string) string {
	for len(s) >= 2 && s[0] == '(' && closingParen(s) == len(s)-1 {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// closingParen returns the index of the parenthesis closing s[0], or -1.
func closingParen(s string) int {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// topLevelSplit splits s at sep where sep is outside parentheses and
// quotes. Unbalanced input is returned whole.
func topLevelSplit(s, sep string) []string {
	var parts []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			continue
		case c == '\'' || c == '"':
			quote = c
			continue
		case c == '(':
			depth++
		case c == ')':
			depth--
		}
		if depth == 0 && strings.HasPrefix(s[i:], sep) {
			parts = append(parts, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	if depth != 0 || quote != 0 {
		return []string{s}
	}
	return append(parts, s[start:])
}
