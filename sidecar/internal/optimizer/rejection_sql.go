package optimizer

import (
	"sort"
	"strings"
)

// canonicalSQL re-spells a normalizeFragment'ed SQL fragment so equivalent
// spellings compare equal: whitespace is kept only between two words (or
// literals) and between two operator characters (so "- -1" never fuses
// into a comment), quoted identifiers that need no quotes lose them, and
// "!=" is spelled "<>". String literals are copied verbatim. The result is
// for comparison and display only; it is never executed.
func canonicalSQL(frag string) string {
	var b strings.Builder
	space := false
	var last byte
	for i := 0; i < len(frag); {
		tok, end := sqlToken(frag, i)
		i = end
		if strings.TrimSpace(tok) == "" {
			space = true
			continue
		}
		if space && last != 0 && needsSpace(last, tok[0]) {
			b.WriteByte(' ')
		}
		space = false
		b.WriteString(tok)
		last = tok[len(tok)-1]
	}
	return b.String()
}

// sqlToken returns the token starting at i (in canonical spelling) and the
// index just past it.
func sqlToken(s string, i int) (string, int) {
	c := s[i]
	switch {
	case c == ' ' || c == '\t' || c == '\n' || c == '\r':
		return " ", i + 1
	case c == '\'':
		end := skipQuoted(s, i, '\'')
		return s[i:end], end
	case c == '"':
		end := skipQuoted(s, i, '"')
		return canonicalIdent(unquoteIdent(s[i:end])), end
	case isIdentByte(c):
		end := i
		for end < len(s) && isIdentByte(s[end]) {
			end++
		}
		return s[i:end], end
	case c == '!' && i+1 < len(s) && s[i+1] == '=':
		return "<>", i + 2
	}
	return s[i : i+1], i + 1
}

func needsSpace(prev, next byte) bool {
	wordish := func(c byte) bool { return isIdentByte(c) || c == '\'' || c == '"' }
	op := func(c byte) bool { return strings.IndexByte("+-*/<>=~!@#%^&|`?:", c) >= 0 }
	return wordish(prev) && wordish(next) || op(prev) && op(next)
}

// stripOuterParens removes parentheses that enclose the whole fragment.
func stripOuterParens(s string) string {
	for len(s) >= 2 && s[0] == '(' && closingParen(s, 0) == len(s)-1 {
		s = s[1 : len(s)-1]
	}
	return s
}

// closingParen returns the index of the parenthesis closing the one at
// open, or -1.
func closingParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'', '"':
			i = skipQuoted(s, i, c) - 1
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// canonicalPredicate normalizes a partial-index predicate: canonical
// spelling, no enclosing parentheses, and top-level AND terms sorted. A
// predicate with a top-level OR or BETWEEN keeps its order (AND binds
// tighter than OR; BETWEEN's AND is not a conjunction).
func canonicalPredicate(where string) string {
	if strings.TrimSpace(where) == "" {
		return ""
	}
	terms := conjuncts(canonicalSQL(where))
	if len(terms) == 1 {
		return terms[0]
	}
	for i, t := range terms {
		if hasTopLevelWord(t, "or") {
			terms[i] = "(" + t + ")"
		}
	}
	sort.Strings(terms)
	return strings.Join(terms, " and ")
}

// conjuncts flattens the top-level AND terms of p.
func conjuncts(p string) []string {
	p = stripOuterParens(p)
	if hasTopLevelWord(p, "or") || hasTopLevelWord(p, "between") {
		return []string{p}
	}
	parts := splitTopLevelWord(p, "and")
	if len(parts) == 1 {
		return parts
	}
	var out []string
	for _, part := range parts {
		out = append(out, conjuncts(part)...)
	}
	return out
}

func hasTopLevelWord(s, word string) bool {
	return len(splitTopLevelWord(s, word)) > 1
}

// splitTopLevelWord splits s at the word outside parentheses and quotes.
func splitTopLevelWord(s, word string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			i = skipQuoted(s, i, c)
			continue
		case c == '(':
			depth++
		case c == ')':
			depth--
		case isIdentByte(c):
			end := i
			for end < len(s) && isIdentByte(s[end]) {
				end++
			}
			if depth == 0 && s[i:end] == word {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = end
			}
			i = end
			continue
		}
		i++
	}
	return append(parts, strings.TrimSpace(s[start:]))
}
