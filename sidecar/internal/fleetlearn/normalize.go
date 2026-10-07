package fleetlearn

import "strings"

// NormalizeQuery reduces a statement to its shape: SQL keywords in lower
// case, every literal and parameter as "?", every identifier (quoted or
// not) as "x", comments dropped, IN lists collapsed, tokens separated by
// one space. Nothing of the data or the schema's names survives.
func NormalizeQuery(sql string) string {
	toks := tokenize(sql)
	return strings.Join(collapseInLists(toks), " ")
}

func tokenize(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		tok, next := nextToken(s, i)
		if tok != "" {
			out = append(out, tok)
		}
		i = next
	}
	return out
}

// nextToken returns the token starting at i ("" for skipped input) and
// the index after it.
func nextToken(s string, i int) (string, int) {
	c := s[i]
	switch {
	case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
		return "", i + 1
	case strings.HasPrefix(s[i:], "--"):
		return "", skipLine(s, i)
	case strings.HasPrefix(s[i:], "/*"):
		return "", skipBlockComment(s, i)
	case c == '\'':
		return "?", skipQuoted(s, i+1, '\'', false)
	case c == '"':
		return "x", skipQuoted(s, i+1, '"', false)
	case c == '$':
		return dollarToken(s, i)
	case isDigit(c) || (c == '.' && i+1 < len(s) && isDigit(s[i+1])):
		return "?", skipNumber(s, i)
	case isIdentStart(c):
		return wordToken(s, i)
	}
	return operatorToken(s, i)
}

func skipLine(s string, i int) int {
	if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
		return i + j + 1
	}
	return len(s)
}

func skipBlockComment(s string, i int) int {
	depth := 0
	for j := i; j < len(s)-1; j++ {
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

// skipQuoted returns the index after the closing quote of a literal
// starting at i (after the opening quote); doubled quotes, and backslash
// escapes when escapes is set, stay inside. Unterminated: end of input.
func skipQuoted(s string, i int, quote byte, escapes bool) int {
	for j := i; j < len(s); j++ {
		switch {
		case escapes && s[j] == '\\':
			j++
		case s[j] == quote && j+1 < len(s) && s[j+1] == quote:
			j++
		case s[j] == quote:
			return j + 1
		}
	}
	return len(s)
}

// dollarToken handles $1 parameters and $tag$...$tag$ literals.
func dollarToken(s string, i int) (string, int) {
	j := i + 1
	for j < len(s) && isDigit(s[j]) {
		j++
	}
	if j > i+1 {
		return "?", j
	}
	for j < len(s) && (isIdentStart(s[j]) || isDigit(s[j])) {
		j++
	}
	if j < len(s) && s[j] == '$' {
		tag := s[i : j+1]
		if end := strings.Index(s[j+1:], tag); end >= 0 {
			return "?", j + 1 + end + len(tag)
		}
		return "?", len(s)
	}
	return "$", i + 1
}

func skipNumber(s string, i int) int {
	j := i
	for j < len(s) && (isDigit(s[j]) || s[j] == '.') {
		j++
	}
	if j < len(s) && (s[j] == 'e' || s[j] == 'E') {
		k := j + 1
		if k < len(s) && (s[k] == '+' || s[k] == '-') {
			k++
		}
		if k < len(s) && isDigit(s[k]) {
			j = k
			for j < len(s) && isDigit(s[j]) {
				j++
			}
		}
	}
	return j
}

// wordToken is a keyword, an identifier ("x") or a prefixed string
// literal such as E'...' or B'...' ("?").
func wordToken(s string, i int) (string, int) {
	j := i
	for j < len(s) && (isIdentStart(s[j]) || isDigit(s[j]) || s[j] == '$') {
		j++
	}
	word := strings.ToLower(s[i:j])
	if j < len(s) && s[j] == '\'' && len(word) == 1 && strings.Contains("ebxn", word) {
		return "?", skipQuoted(s, j+1, '\'', word == "e")
	}
	if sqlKeywords[word] {
		return word, j
	}
	return "x", j
}

var multiCharOps = []string{"::", "<=", ">=", "<>", "!=", "||", "->>", "->", "#>>",
	"#>", "@>", "<@", "&&"}

func operatorToken(s string, i int) (string, int) {
	for _, op := range multiCharOps {
		if strings.HasPrefix(s[i:], op) {
			return op, i + len(op)
		}
	}
	return string(s[i]), i + 1
}

// collapseInLists turns "in ( ? , ? , ? )" into "in ( ? )".
func collapseInLists(toks []string) []string {
	out := make([]string, 0, len(toks))
	for i := 0; i < len(toks); i++ {
		out = append(out, toks[i])
		if toks[i] != "in" || i+2 >= len(toks) || toks[i+1] != "(" || toks[i+2] != "?" {
			continue
		}
		j := i + 3
		for j+1 < len(toks) && toks[j] == "," && toks[j+1] == "?" {
			j += 2
		}
		if j < len(toks) && toks[j] == ")" {
			out = append(out, "(", "?", ")")
			i = j
		}
	}
	return out
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}
