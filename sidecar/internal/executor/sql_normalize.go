package executor

import (
	"fmt"
	"strings"
	"unicode"
)

// sqlToken scanning state shared by the normalizers below. Single-quoted
// literals and double-quoted identifiers are copied verbatim; everything
// outside them is subject to whitespace/comment handling.
type sqlScanner struct {
	src   []rune
	pos   int
	quote rune
}

// normalizeSQLText collapses whitespace runs outside literals to one space
// and replaces comments with a space, so prefix-based classification cannot
// be bypassed with "VACUUM  FULL", tabs, newlines or "/**/".
func normalizeSQLText(sql string) string {
	out, _ := scanSQL(sql)
	return strings.TrimSpace(out)
}

// hasSQLComment reports a -- or /* comment outside literals and identifiers.
func hasSQLComment(sql string) bool {
	_, comment := scanSQL(sql)
	return comment
}

func scanSQL(sql string) (string, bool) {
	scanner := sqlScanner{src: []rune(sql)}
	var out strings.Builder
	comment, pendingSpace := false, false
	for scanner.pos < len(scanner.src) {
		r := scanner.src[scanner.pos]
		if scanner.quote != 0 {
			scanner.copyQuoted(&out, r)
			continue
		}
		if skipped := scanner.skipComment(); skipped {
			comment, pendingSpace = true, true
			continue
		}
		scanner.pos++
		if unicode.IsSpace(r) {
			pendingSpace = true
			continue
		}
		if pendingSpace && out.Len() > 0 {
			out.WriteByte(' ')
		}
		pendingSpace = false
		if r == '\'' || r == '"' {
			scanner.quote = r
		}
		out.WriteRune(r)
	}
	return out.String(), comment
}

func (s *sqlScanner) copyQuoted(out *strings.Builder, r rune) {
	out.WriteRune(r)
	s.pos++
	if r != s.quote {
		return
	}
	if s.pos < len(s.src) && s.src[s.pos] == s.quote {
		out.WriteRune(r) // doubled quote is an escaped quote character
		s.pos++
		return
	}
	s.quote = 0
}

func (s *sqlScanner) skipComment() bool {
	if s.pos+1 >= len(s.src) {
		return false
	}
	pair := string(s.src[s.pos : s.pos+2])
	switch pair {
	case "--":
		for s.pos < len(s.src) && s.src[s.pos] != '\n' {
			s.pos++
		}
		return true
	case "/*":
		s.pos += 2
		for s.pos+1 < len(s.src) && string(s.src[s.pos:s.pos+2]) != "*/" {
			s.pos++
		}
		s.pos += 2
		return true
	}
	return false
}

// closingParenOutsideQuotes returns the index of the parenthesis closing the
// one at open, honouring single-quoted literals and quoted identifiers.
func closingParenOutsideQuotes(s string, open int) int {
	depth := 0
	var quote byte
	for i := open; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
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

// requireSingleReloptionSubcmd rejects ALTER TABLE action lists such as
// "SET (autovacuum_...), DROP COLUMN x": nothing may follow the closing
// parenthesis of the storage-parameter list except a terminating semicolon.
func requireSingleReloptionSubcmd(sub string) error {
	open := strings.IndexByte(sub, '(')
	closing := closingParenOutsideQuotes(sub, open)
	if open < 0 || closing < 0 {
		return fmt.Errorf("%w: malformed ALTER TABLE storage parameters", ErrDisallowedSQL)
	}
	rest := strings.TrimSpace(sub[closing+1:])
	if rest != "" && rest != ";" {
		return fmt.Errorf(
			"%w: ALTER TABLE must contain exactly one storage-parameter sub-command",
			ErrDisallowedSQL)
	}
	return nil
}
