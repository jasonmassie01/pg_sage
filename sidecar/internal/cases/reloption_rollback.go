package cases

import "strings"

// capturedReloptionRollback trusts a finding's rollback (vacuum-07) only
// when it is a single ALTER TABLE ... SET/RESET (...) of the same table
// as the change.
func capturedReloptionRollback(change, rollback string) (string, bool) {
	rollback = strings.TrimSpace(rollback)
	target, ok := alterTableTarget(change)
	if !ok || rollback == "" {
		return "", false
	}
	if other, ok := alterTableTarget(rollback); !ok || other != target {
		return "", false
	}
	return rollback, true
}

// alterTableTarget is the table a single ALTER TABLE <name> SET (...) or
// RESET (...) names. Anything more (a second subcommand after a comma, a
// second statement, a nested parenthesis) has none: ALTER TABLE takes
// several subcommands, and only a reloption change is a trusted inverse.
func alterTableTarget(sql string) (string, bool) {
	sql = strings.TrimSuffix(strings.TrimSpace(sql), ";")
	rest, ok := strings.CutPrefix(sql, "ALTER TABLE ")
	if !ok {
		return "", false
	}
	name, rest, ok := cutQualifiedName(rest)
	if !ok {
		return "", false
	}
	list, ok := strings.CutPrefix(rest, " SET (")
	if !ok {
		if list, ok = strings.CutPrefix(rest, " RESET ("); !ok {
			return "", false
		}
	}
	end, ok := closingParen(list)
	if !ok || end != len(list)-1 {
		return "", false
	}
	return name, true
}

// cutQualifiedName reads a dotted name of quoted or plain identifiers.
func cutQualifiedName(s string) (string, string, bool) {
	i := 0
	for {
		n, ok := identifierLength(s[i:])
		if !ok {
			return "", "", false
		}
		i += n
		if i >= len(s) || s[i] != '.' {
			return s[:i], s[i:], true
		}
		i++
	}
}

// identifierLength is the length of the identifier s starts with: a
// double-quoted one ("" escapes a quote) or a run of plain characters.
func identifierLength(s string) (int, bool) {
	if strings.HasPrefix(s, `"`) {
		n, ok := quotedLength(s, '"')
		return n, ok && n > 2
	}
	n := 0
	for n < len(s) && (s[n] == '_' || s[n] == '$' || s[n] >= '0' && s[n] <= '9' ||
		s[n]|0x20 >= 'a' && s[n]|0x20 <= 'z') {
		n++
	}
	return n, n > 0
}

// quotedLength is the length of the quoted token s starts with, its
// closing quote included; a doubled quote is an escaped one.
func quotedLength(s string, quote byte) (int, bool) {
	for i := 1; i < len(s); i++ {
		if s[i] != quote {
			continue
		}
		if i+1 < len(s) && s[i+1] == quote {
			i++
			continue
		}
		return i + 1, true
	}
	return 0, false
}

// closingParen is the index of the ')' that closes a reloption list,
// skipping quoted values; a nested '(' or a ';' outside quotes has none.
func closingParen(s string) (int, bool) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'', '"':
			n, ok := quotedLength(s[i:], s[i])
			if !ok {
				return 0, false
			}
			i += n - 1
		case '(', ';':
			return 0, false
		case ')':
			return i, true
		}
	}
	return 0, false
}
