package tuning

import (
	"errors"
	"strings"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// Identifiers: catalog names are folded like PostgreSQL folds them, and
// rendered by sqlident, quoted only when they must be (upper case, special
// characters or a key word that the server's quote_ident quotes), so
// "public.orders" and `"Sales"."Orders"` are the canonical forms of the
// tables the agent names.

var errBadIdent = errors.New("not an identifier")

// readIdent reads one identifier at s[i:]: a quoted one exactly, a bare one
// folded to lower case. It returns the name and the index after it.
func readIdent(s string, i int) (string, int, error) {
	if i >= len(s) {
		return "", i, errBadIdent
	}
	if s[i] == '"' {
		var b strings.Builder
		for j := i + 1; j < len(s); j++ {
			if s[j] != '"' {
				b.WriteByte(s[j])
				continue
			}
			if j+1 < len(s) && s[j+1] == '"' {
				b.WriteByte('"')
				j++
				continue
			}
			if b.Len() == 0 {
				return "", j + 1, errBadIdent
			}
			return b.String(), j + 1, nil
		}
		return "", len(s), errBadIdent
	}
	j := i
	for j < len(s) && isIdentByte(s[j], j == i) {
		j++
	}
	if j == i {
		return "", i, errBadIdent
	}
	return strings.ToLower(s[i:j]), j, nil
}

func isIdentByte(c byte, first bool) bool {
	letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 0x80
	if first {
		return letter
	}
	return letter || c >= '0' && c <= '9' || c == '$'
}

// readQualified reads "name" or "schema.name" at s[i:]; schema is empty
// for an unqualified name.
func readQualified(s string, i int) (schema, name string, next int, err error) {
	first, j, err := readIdent(s, i)
	if err != nil {
		return "", "", j, err
	}
	k := skipSpace(s, j)
	if k < len(s) && s[k] == '.' {
		second, l, err := readIdent(s, skipSpace(s, k+1))
		if err != nil {
			return "", "", l, err
		}
		return first, second, l, nil
	}
	return "", first, j, nil
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

// splitQualified parses a whole "schema.name" reference; both parts are
// required and nothing may follow.
func splitQualified(ref string) (schema, name string, ok bool) {
	s := strings.TrimSpace(ref)
	schema, name, next, err := readQualified(s, 0)
	if err != nil || schema == "" || skipSpace(s, next) != len(s) {
		return "", "", false
	}
	return schema, name, true
}

// canonicalRef is ref in canonical form, or "" when it is not a qualified
// name.
func canonicalRef(ref string) string {
	schema, name, ok := splitQualified(ref)
	if !ok {
		return ""
	}
	return sqlident.Qualified(schema, name)
}
