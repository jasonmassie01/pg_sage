package planstability

import "strings"

// token is one lexical token of a statement. An identifier's text is its
// resolved name (bare folded to lower case, quoted unescaped); a literal
// or dollar body is "'"; comments and whitespace are dropped.
type token struct {
	text   string
	ident  bool
	quoted bool
}

const operatorChars = "<>=!~+-*/%^&|#@?"

// tokenize splits text that sqlast.ScanSQL already read in full, so every
// quote and comment it meets is terminated.
func tokenize(text string) []token {
	var out []token
	for i := 0; i < len(text); {
		t, next := nextToken(text, i)
		if t.text != "" {
			out = append(out, t)
		}
		i = next
	}
	return out
}

// nextToken reads the token at i and returns where the next one starts.
func nextToken(s string, i int) (token, int) {
	c := s[i]
	switch {
	case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
		return token{}, i + 1
	case strings.HasPrefix(s[i:], "--"):
		if end := strings.IndexByte(s[i:], '\n'); end >= 0 {
			return token{}, i + end + 1
		}
		return token{}, len(s)
	case strings.HasPrefix(s[i:], "/*"):
		return token{}, commentEnd(s, i)
	case c == '\'':
		return token{text: "'"}, quotedEnd(s, i+1, '\'', false)
	case (c == 'E' || c == 'e') && i+1 < len(s) && s[i+1] == '\'':
		return token{text: "'"}, quotedEnd(s, i+2, '\'', true)
	case c == '"':
		end := quotedEnd(s, i+1, '"', false)
		name := strings.ReplaceAll(s[i+1:end-1], `""`, `"`)
		return token{text: name, ident: true, quoted: true}, end
	case c == '$':
		return dollarToken(s, i)
	case isIdentStart(c):
		end := i + 1
		for end < len(s) && (isIdentStart(s[end]) || s[end] >= '0' && s[end] <= '9' ||
			s[end] == '$') {
			end++
		}
		return token{text: strings.ToLower(s[i:end]), ident: true}, end
	case c >= '0' && c <= '9':
		end := i + 1
		for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == '.') {
			end++
		}
		return token{text: s[i:end]}, end
	case strings.IndexByte(operatorChars, c) >= 0:
		end := i + 1
		for end < len(s) && strings.IndexByte(operatorChars, s[end]) >= 0 {
			end++
		}
		return token{text: s[i:end]}, end
	}
	return token{text: s[i : i+1]}, i + 1
}

// dollarToken is a $n parameter or a $tag$ ... $tag$ body.
func dollarToken(s string, i int) (token, int) {
	end := i + 1
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end > i+1 {
		return token{text: s[i:end]}, end
	}
	close := strings.IndexByte(s[i+1:], '$')
	if close < 0 {
		return token{text: "$"}, i + 1
	}
	tag := s[i : i+close+2]
	body := strings.Index(s[i+len(tag):], tag)
	if body < 0 {
		return token{text: "'"}, len(s)
	}
	return token{text: "'"}, i + len(tag) + body + len(tag)
}

// quotedEnd is the index after the closing quote of a body starting at i;
// a doubled quote (or, in an E-string, a backslash) escapes.
func quotedEnd(s string, i int, quote byte, backslash bool) int {
	for i < len(s) {
		switch {
		case backslash && s[i] == '\\':
			i += 2
		case s[i] == quote && i+1 < len(s) && s[i+1] == quote:
			i += 2
		case s[i] == quote:
			return i + 1
		default:
			i++
		}
	}
	return len(s)
}

// commentEnd is the index after a (nested) block comment at i.
func commentEnd(s string, i int) int {
	depth := 0
	for i < len(s) {
		switch {
		case strings.HasPrefix(s[i:], "/*"):
			depth, i = depth+1, i+2
		case strings.HasPrefix(s[i:], "*/"):
			depth, i = depth-1, i+2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(s)
}

func isIdentStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 0x80
}
