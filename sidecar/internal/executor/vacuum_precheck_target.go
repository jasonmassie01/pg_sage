package executor

import (
	"strings"
	"unicode"
)

// maintenanceTable is the one relation a prechecked VACUUM or ALTER TABLE
// names, read quote-aware so a quoted identifier holding spaces or commas
// stays whole. It is "" when the statement names no relation or more than
// one ("VACUUM a, b"): the precheck then refuses, as checking only the
// first relation would let the server skip the others with a WARNING.
func maintenanceTable(sql string) string {
	tokens := relationTokens(normalizeSQLText(sql))
	var i int
	switch {
	case len(tokens) > 0 && strings.EqualFold(tokens[0], "VACUUM"):
		i = skipParenGroup(tokens, 1)
		i = skipWords(tokens, i, "FULL", "FREEZE", "VERBOSE", "ANALYZE", "ONLY")
	case len(tokens) > 1 && strings.EqualFold(tokens[0], "ALTER") &&
		strings.EqualFold(tokens[1], "TABLE"):
		i = skipWords(tokens, 2, "IF", "EXISTS", "ONLY")
	default:
		return ""
	}
	if i >= len(tokens) || isRelationPunct(tokens[i]) {
		return ""
	}
	table := tokens[i]
	if next := skipParenGroup(tokens, i+1); next < len(tokens) && tokens[next] == "," {
		return ""
	}
	return table
}

// relationTokens splits sql into words and the punctuation ( ) , ; with a
// quoted identifier or literal (doubled quotes included) kept inside its
// word.
func relationTokens(sql string) []string {
	var tokens []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			tokens = append(tokens, word.String())
			word.Reset()
		}
	}
	var quote rune
	for _, r := range sql {
		switch {
		case quote != 0:
			word.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
			word.WriteRune(r)
		case unicode.IsSpace(r):
			flush()
		case isRelationPunct(string(r)):
			flush()
			tokens = append(tokens, string(r))
		default:
			word.WriteRune(r)
		}
	}
	flush()
	return tokens
}

func isRelationPunct(token string) bool {
	return token == "(" || token == ")" || token == "," || token == ";"
}

// skipParenGroup returns the index past a parenthesized group starting at
// tokens[i], or i when none starts there.
func skipParenGroup(tokens []string, i int) int {
	if i >= len(tokens) || tokens[i] != "(" {
		return i
	}
	depth := 0
	for ; i < len(tokens); i++ {
		switch tokens[i] {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return i
}

// skipWords returns the index past the unquoted keywords at tokens[i:].
func skipWords(tokens []string, i int, words ...string) int {
	for i < len(tokens) && containsFold(words, tokens[i]) {
		i++
	}
	return i
}
