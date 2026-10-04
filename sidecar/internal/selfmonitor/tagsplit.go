package selfmonitor

import "strings"

// placeTags tags every statement of a simple Query's text (placeTag on
// each): pg_stat_statements stores each statement of a multi-statement
// query with its own text, so a tag on the first statement marks only
// that one. Text inside literals, quoted identifiers, dollar quotes and
// comments is never split. When the boundaries cannot be read with
// certainty (statementEnds), only the first statement is tagged.
func placeTags(sql string) string {
	ends, ok := statementEnds(sql)
	if !ok || len(ends) == 0 {
		return placeTag(sql)
	}
	var b strings.Builder
	b.Grow(len(sql) + (len(ends)+1)*(len(StatementTag)+2))
	start := 0
	for _, end := range append(ends, len(sql)) {
		segment := sql[start:end]
		if emptyStatement(segment) {
			b.WriteString(segment)
		} else {
			b.WriteString(placeTag(segment))
		}
		start = end
	}
	return b.String()
}

// emptyStatement reports a segment with no token before its ';' (white
// space and comments only), which is left as it is.
func emptyStatement(segment string) bool {
	i, _, _, _, ok := scanLeading(segment)
	return ok && (i == len(segment) || segment[i] == ';')
}

// statementEnds returns the offset just past every ';' that ends a
// statement (outside parentheses, literals, quoted identifiers, dollar
// quotes and comments). ok is false when the text cannot be read with
// certainty: an unterminated quote or comment, or a backslash in a quoted
// literal, whose meaning depends on the server's standard_conforming_strings
// (and on an E prefix).
func statementEnds(sql string) (ends []int, ok bool) {
	depth := 0
	for i := 0; i < len(sql); {
		n := tokenLen(sql[i:])
		if n < 0 {
			return nil, false
		}
		switch sql[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ';':
			if depth == 0 {
				ends = append(ends, i+1)
			}
		}
		i += n
	}
	return ends, true
}

// tokenLen is the length of the lexical unit s starts with, as far as
// statement boundaries need it: a literal, quoted identifier, dollar
// quote, comment or identifier as a whole, anything else one byte; -1
// when it cannot be read with certainty.
func tokenLen(s string) int {
	switch c := s[0]; {
	case c == '\'':
		return quotedLen(s, '\'')
	case c == '"':
		return quotedLen(s, '"')
	case c == '$':
		return dollarLen(s)
	case strings.HasPrefix(s, "--"):
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			return nl + 1
		}
		return len(s)
	case strings.HasPrefix(s, "/*"):
		return commentLen(s)
	case isIdentStart(c):
		return identLen(s)
	default:
		return 1
	}
}

// quotedLen is the length of the quoted literal or identifier s starts
// with, or -1 when it is unterminated or, for a literal, holds a
// backslash. A doubled quote inside ('it''s') reads as two adjacent
// quoted units, which leaves the statement boundaries the same.
func quotedLen(s string, quote byte) int {
	for j := 1; j < len(s); j++ {
		switch {
		case s[j] == quote:
			return j + 1
		case s[j] == '\\' && quote == '\'':
			return -1
		}
	}
	return -1
}

// dollarLen is the length of the dollar-quoted string s starts with
// ($$...$$ or $tag$...$tag$; a tag does not start with a digit), -1 when
// it is unterminated, or 1 for any other '$' (a parameter such as $1).
func dollarLen(s string) int {
	j := 1
	for j < len(s) && isIdentChar(s[j]) && (j > 1 || !isDigit(s[j])) {
		j++
	}
	if j >= len(s) || s[j] != '$' {
		return 1
	}
	delim := s[:j+1]
	end := strings.Index(s[j+1:], delim)
	if end < 0 {
		return -1
	}
	return j + 1 + end + len(delim)
}

// identLen is the length of the identifier or keyword s starts with; it
// may hold digits and '$' after its first character.
func identLen(s string) int {
	j := 1
	for j < len(s) && (isIdentChar(s[j]) || s[j] == '$') {
		j++
	}
	return j
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 0x80 || (c|0x20 >= 'a' && c|0x20 <= 'z')
}

func isIdentChar(c byte) bool { return isIdentStart(c) || isDigit(c) }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
