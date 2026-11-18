package agenttools

import (
	"regexp"
	"strings"
)

// codeLiteral is one string literal of application code and the line its
// opening quote is on.
type codeLiteral struct {
	text string
	line int
}

// sqlVerb finds literals worth fingerprinting.
var sqlVerb = regexp.MustCompile(`(?i)\b(select|insert|update|delete|with)\b`)

// heredocStart is a Ruby, shell or PHP heredoc opener with an upper-case tag.
var heredocStart = regexp.MustCompile(`^<<<?[~-]?(['"]?)([A-Z_][A-Z0-9_]*)(['"]?)`)

// codeLiterals returns the string literals of source code that look like
// SQL, in any of the common languages: '...' and "..." on one line with
// backslash escapes, backquoted and triple-quoted (double or single)
// literals across lines, and heredocs (<<~SQL ... SQL). It is lexical
// only; a literal built by concatenation is not reassembled.
func codeLiterals(src string) []codeLiteral {
	var out []codeLiteral
	line := 1
	for i := 0; i < len(src); {
		text, next, ok := literalAt(src, i)
		if ok && sqlVerb.MatchString(text) {
			out = append(out, codeLiteral{text: text, line: line})
		}
		if next <= i {
			next = i + 1
		}
		line += strings.Count(src[i:next], "\n")
		i = next
	}
	return out
}

// literalAt reads the literal starting at i, if one does, and returns
// where scanning continues.
func literalAt(src string, i int) (string, int, bool) {
	rest := src[i:]
	switch {
	case strings.HasPrefix(rest, `"""`), strings.HasPrefix(rest, "'''"):
		end := strings.Index(rest[3:], rest[:3])
		if end < 0 {
			return "", i + 3, false
		}
		return rest[3 : 3+end], i + 3 + end + 3, true
	case rest[0] == '`':
		end := strings.IndexByte(rest[1:], '`')
		if end < 0 {
			return "", i + 1, false
		}
		return rest[1 : 1+end], i + 1 + end + 1, true
	case rest[0] == '"' || rest[0] == '\'':
		return oneLineLiteral(src, i)
	case strings.HasPrefix(rest, "<<"):
		return heredocAt(src, i)
	}
	return "", i + 1, false
}

// oneLineLiteral reads a quoted literal that ends on its own line; an
// apostrophe in a comment ("don't") opens nothing.
func oneLineLiteral(src string, i int) (string, int, bool) {
	quote := src[i]
	var b strings.Builder
	for j := i + 1; j < len(src); j++ {
		switch c := src[j]; {
		case c == '\n':
			return "", i + 1, false
		case c == '\\' && j+1 < len(src):
			j++
			b.WriteByte(unescape(src[j]))
		case c == quote:
			return b.String(), j + 1, true
		default:
			b.WriteByte(c)
		}
	}
	return "", i + 1, false
}

// unescape maps the character after a backslash; layout escapes become a
// space, which a fingerprint ignores.
func unescape(c byte) byte {
	switch c {
	case 'n', 't', 'r':
		return ' '
	}
	return c
}

// heredocAt reads a heredoc body: the lines after the opener up to a line
// that is only the tag.
func heredocAt(src string, i int) (string, int, bool) {
	m := heredocStart.FindStringSubmatch(src[i:])
	if m == nil || m[1] != m[3] {
		return "", i + 2, false
	}
	bodyStart := endOfLine(src, i)
	for j := bodyStart; j < len(src); {
		end := endOfLine(src, j)
		if strings.TrimSpace(src[j:end]) == m[2] {
			return src[bodyStart:j], end, true
		}
		j = end
	}
	return "", i + 2, false
}
