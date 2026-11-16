package workpack

import "strings"

// The scanners of the leak guard: string literals of SQL text, the words
// of SQL text outside its literals, and the elements of a PostgreSQL array
// literal (pg_stats' anyarray columns as text).

// stringLiterals is every '...', E'...' and $tag$...$tag$ body of s.
func stringLiterals(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		body, end, ok := literalAt(s, i)
		if ok {
			out = append(out, body)
			i = end
			continue
		}
		i++
	}
	return out
}

// literalAt reads the literal starting at i: its body and the index after
// it, or false when none starts there.
func literalAt(s string, i int) (string, int, bool) {
	switch {
	case s[i] == '\'':
		escapes := i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') &&
			(i == 1 || !isIdentByte(s[i-2]))
		body, end := quotedBody(s, i, escapes)
		return body, end, true
	case s[i] == '$':
		j := i + 1
		for j < len(s) && (isIdentByte(s[j]) && s[j] != '$') {
			j++
		}
		if j >= len(s) || s[j] != '$' || (j > i+1 && s[i+1] >= '0' && s[i+1] <= '9') {
			return "", 0, false
		}
		tag := s[i : j+1]
		rest := s[j+1:]
		k := strings.Index(rest, tag)
		if k < 0 {
			return rest, len(s), true
		}
		return rest[:k], j + 1 + k + len(tag), true
	}
	return "", 0, false
}

// quotedBody reads the single-quoted literal at i (a doubled quote is one
// quote, and with escapes a backslash escapes the next byte).
func quotedBody(s string, i int, escapes bool) (string, int) {
	var b strings.Builder
	j := i + 1
	for j < len(s) {
		switch {
		case escapes && s[j] == '\\' && j+1 < len(s):
			b.WriteByte(s[j+1])
			j += 2
			continue
		case s[j] == '\'' && j+1 < len(s) && s[j+1] == '\'':
			b.WriteByte('\'')
			j += 2
			continue
		case s[j] == '\'':
			return b.String(), j + 1
		}
		b.WriteByte(s[j])
		j++
	}
	return b.String(), j
}

// wordsOutsideLiterals is every identifier-like word of s that is not
// inside a literal (double-quoted identifiers included, unquoted).
func wordsOutsideLiterals(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		if _, end, ok := literalAt(s, i); ok {
			i = end
			continue
		}
		switch {
		case s[i] == '"':
			inner, end := quotedIdent(s, i)
			out = append(out, inner)
			i = end
		case s[i] == '$': // a $n parameter
			i++
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
		case isIdentByte(s[i]):
			j := i
			for j < len(s) && isIdentByte(s[j]) && s[j] != '$' {
				j++
			}
			out = append(out, s[i:j])
			i = j
		default:
			i++
		}
	}
	return out
}

// pgArrayElements is the elements of a one-dimensional array literal
// ({a,"b c",NULL}); NULL elements are left out.
func pgArrayElements(s string) []string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil
	}
	s = s[1 : len(s)-1]
	var out []string
	for i := 0; i < len(s); {
		var el string
		quoted := s[i] == '"'
		if quoted {
			var b strings.Builder
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j++
				}
				b.WriteByte(s[j])
				j++
			}
			el, i = b.String(), j+1
		} else {
			j := strings.IndexByte(s[i:], ',')
			if j < 0 {
				j = len(s) - i
			}
			el, i = strings.TrimSpace(s[i:i+j]), i+j
		}
		if quoted || el != "NULL" {
			out = append(out, el)
		}
		for i < len(s) && s[i] != ',' {
			i++
		}
		i++
	}
	return out
}
