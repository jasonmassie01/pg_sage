package executor

import "strings"

// isSingleRelationVacuum reports a VACUUM that names exactly one relation
// (with an optional column list) past its option list and bare option
// keywords. A bare VACUUM, VACUUM (opts) and a comma list vacuum the
// whole database or several tables: they are not vacuum_table and have
// no contract (executor-08). upper is the normalized, upper-cased SQL.
func isSingleRelationVacuum(upper string) bool {
	rest := strings.TrimSpace(strings.TrimPrefix(upper, "VACUUM"))
	if strings.HasPrefix(rest, "(") {
		end := closingParen(rest)
		if end < 0 {
			return false
		}
		rest = strings.TrimSpace(rest[end+1:])
	}
	for {
		word, tail, _ := strings.Cut(rest, " ")
		switch word {
		case "FULL", "FREEZE", "VERBOSE", "ANALYZE":
			rest = strings.TrimSpace(tail)
			continue
		}
		break
	}
	rest = strings.TrimSpace(strings.TrimSuffix(rest, ";"))
	return rest != "" && isOneRelation(rest)
}

// isOneRelation reports a single (possibly qualified, possibly quoted)
// relation name, optionally followed by one parenthesized column list.
func isOneRelation(text string) bool {
	inQuote, columns := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case inQuote:
		case c == '(':
			end := closingParen(text[i:])
			if columns || end < 0 || strings.TrimSpace(text[i+end+1:]) != "" {
				return false
			}
			columns = true
			i += end
		case c == ',' || c == ';':
			return false
		case c == ' ':
			if !strings.HasPrefix(strings.TrimSpace(text[i:]), "(") {
				return false
			}
		}
	}
	return !inQuote
}

// closingParen is the index of the parenthesis closing the one that
// opens text, outside quoted identifiers, or -1.
func closingParen(text string) int {
	depth, inQuote := 0, false
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '"':
			inQuote = !inQuote
		case inQuote:
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
