package schema

import "strings"

// A small SQL scanner for pg_sage's own migration constants (config-04):
// it knows comments, quoted strings and identifiers, and dollar quotes,
// which is enough to split a step into statements and to read clauses at
// paren depth zero. It is not a general SQL parser.

// sqlSpan returns the end of the comment, quoted string, quoted identifier
// or dollar quote starting at s[i], or i when none starts there.
func sqlSpan(s string, i int) int {
	switch {
	case strings.HasPrefix(s[i:], "--"):
		if nl := strings.IndexByte(s[i:], '\n'); nl >= 0 {
			return i + nl + 1
		}
		return len(s)
	case strings.HasPrefix(s[i:], "/*"):
		return blockCommentEnd(s, i)
	case s[i] == '\'' || s[i] == '"':
		return quotedEnd(s, i, s[i])
	case s[i] == '$':
		if tag := dollarTag(s, i); tag != "" {
			if end := strings.Index(s[i+len(tag):], tag); end >= 0 {
				return i + len(tag) + end + len(tag)
			}
			return len(s)
		}
	}
	return i
}

// blockCommentEnd is the end of the (nestable) block comment at s[i].
func blockCommentEnd(s string, i int) int {
	depth := 0
	for j := i; j+1 < len(s); j++ {
		switch {
		case s[j] == '/' && s[j+1] == '*':
			depth++
			j++
		case s[j] == '*' && s[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				return j + 1
			}
		}
	}
	return len(s)
}

// quotedEnd is the end of the q-quoted token at s[i]; a doubled q is an
// escaped quote.
func quotedEnd(s string, i int, q byte) int {
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

// dollarTag returns the dollar-quote tag ($$ or $name$) starting at s[i],
// or "" for a parameter ($1) or a $ inside an identifier.
func dollarTag(s string, i int) string {
	if i > 0 && isIdentByte(s[i-1]) {
		return ""
	}
	for j := i + 1; j < len(s); j++ {
		switch c := s[j]; {
		case c == '$':
			return s[i : j+1]
		case c >= '0' && c <= '9' && j == i+1:
			return ""
		case !isIdentByte(c):
			return ""
		}
	}
	return ""
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' || c >= 0x80
}

// splitSQL splits s at the semicolons outside comments, quotes and dollar
// quotes. The pieces keep their text, so joining them with ";" gives s.
func splitSQL(s string) []string {
	return splitTopLevelOn(s, ';', false)
}

// splitTopLevel splits s at sep outside comments, quotes and parentheses.
func splitTopLevel(s string, sep byte) []string {
	return splitTopLevelOn(s, sep, true)
}

func splitTopLevelOn(s string, sep byte, parens bool) []string {
	var out []string
	start, depth := 0, 0
	for i := 0; i < len(s); {
		if end := sqlSpan(s, i); end > i {
			i = end
			continue
		}
		switch c := s[i]; {
		case parens && c == '(':
			depth++
		case parens && c == ')':
			depth--
		case c == sep && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
		i++
	}
	return append(out, s[start:])
}

// stripSQLComments replaces every comment in s with one space, keeping
// quoted text as it is.
func stripSQLComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		end := sqlSpan(s, i)
		if end == i {
			b.WriteByte(s[i])
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "--") || strings.HasPrefix(s[i:], "/*") {
			b.WriteByte(' ')
		} else {
			b.WriteString(s[i:end])
		}
		i = end
	}
	return b.String()
}
