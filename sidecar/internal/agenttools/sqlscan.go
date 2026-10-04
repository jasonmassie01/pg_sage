package agenttools

import (
	"errors"
	"strings"
)

// errUnterminated is SQL text that ends inside a comment, literal or
// quoted identifier.
var errUnterminated = errors.New("unterminated comment, literal or quoted identifier")

// sqlScan is the lexical structure of SQL text that matters here: the
// block comments outside literals and the statements separated by ';'.
type sqlScan struct {
	comments   []string // block comment bodies, in order
	statements int      // statements with any code (a trailing ';' adds none)
}

// scanSQL walks sql as PostgreSQL lexes it: quoted literals (a doubled
// quote or, in E-strings, a backslash escapes), "..." identifiers, $tag$
// bodies, nested /* */ comments and -- line comments. Text inside them
// is never a comment or a separator. It returns what it saw up to an
// unterminated token, and the error.
func scanSQL(sql string) (sqlScan, error) {
	var out sqlScan
	code := false
	for i := 0; i < len(sql); {
		next, err := scanToken(sql, i, &out, &code)
		if err != nil {
			return out, err
		}
		i = next
	}
	if code {
		out.statements++
	}
	return out, nil
}

// scanToken consumes one token at i and returns where the next starts.
func scanToken(sql string, i int, out *sqlScan, code *bool) (int, error) {
	c := sql[i]
	switch {
	case c == '-' && strings.HasPrefix(sql[i:], "--"):
		if end := strings.IndexByte(sql[i:], '\n'); end >= 0 {
			return i + end + 1, nil
		}
		return len(sql), nil
	case c == '/' && strings.HasPrefix(sql[i:], "/*"):
		end, ok := blockCommentEnd(sql, i)
		if !ok {
			return 0, errUnterminated
		}
		out.comments = append(out.comments, sql[i+2:end-2])
		return end, nil
	case c == ';':
		if *code {
			out.statements++
		}
		*code = false
		return i + 1, nil
	case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
		return i + 1, nil
	}
	*code = true
	return codeTokenEnd(sql, i)
}

// codeTokenEnd consumes a literal, quoted identifier, dollar body or one
// byte of other code.
func codeTokenEnd(sql string, i int) (int, error) {
	switch sql[i] {
	case '\'':
		return quotedEnd(sql, i, '\'', isEscapeString(sql, i))
	case '"':
		return quotedEnd(sql, i, '"', false)
	case '$':
		if tag := dollarTag(sql, i); tag != "" {
			end := strings.Index(sql[i+len(tag):], tag)
			if end < 0 {
				return 0, errUnterminated
			}
			return i + len(tag) + end + len(tag), nil
		}
	}
	return i + 1, nil
}

// blockCommentEnd returns the index after the */ closing the (possibly
// nested) comment opened at i.
func blockCommentEnd(sql string, i int) (int, bool) {
	depth := 0
	for j := i; j+1 < len(sql); {
		switch {
		case sql[j] == '/' && sql[j+1] == '*':
			depth++
			j += 2
		case sql[j] == '*' && sql[j+1] == '/':
			depth--
			j += 2
			if depth == 0 {
				return j, true
			}
		default:
			j++
		}
	}
	return 0, false
}

// quotedEnd returns the index after the quote closing the token opened at
// i; a doubled quote is an escaped one, and so is a backslash escape in an
// E-string.
func quotedEnd(sql string, i int, quote byte, backslash bool) (int, error) {
	for j := i + 1; j < len(sql); j++ {
		switch {
		case backslash && sql[j] == '\\':
			j++
		case sql[j] == quote && j+1 < len(sql) && sql[j+1] == quote:
			j++
		case sql[j] == quote:
			return j + 1, nil
		}
	}
	return 0, errUnterminated
}

func isEscapeString(sql string, i int) bool {
	if i == 0 || (sql[i-1] != 'E' && sql[i-1] != 'e') {
		return false
	}
	return i == 1 || !isWordByte(sql[i-2])
}

// dollarTag returns the $tag$ opening a dollar-quoted body at i, or "".
// "$1" is a parameter, and a '$' inside a word is part of the word.
func dollarTag(sql string, i int) string {
	if i > 0 && isWordByte(sql[i-1]) {
		return ""
	}
	j := i + 1
	for j < len(sql) && (isLetter(sql[j]) || (j > i+1 && sql[j] >= '0' && sql[j] <= '9')) {
		j++
	}
	if j < len(sql) && sql[j] == '$' {
		return sql[i : j+1]
	}
	return ""
}

func isLetter(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isWordByte(c byte) bool {
	return isLetter(c) || (c >= '0' && c <= '9') || c == '$'
}
