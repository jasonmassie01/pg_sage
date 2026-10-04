package tuning

import (
	"errors"
	"strings"
)

// Identifiers: catalog names are folded like PostgreSQL folds them, and
// rendered quoted only when they must be (upper case, special characters
// or a reserved word), so "public.orders" and `"Sales"."Orders"` are the
// canonical forms of the tables the agent names.

// reservedWords are PostgreSQL's reserved key words: an identifier spelled
// like one must be quoted.
var reservedWords = map[string]bool{
	"all": true, "analyse": true, "analyze": true, "and": true, "any": true,
	"array": true, "as": true, "asc": true, "asymmetric": true, "both": true,
	"case": true, "cast": true, "check": true, "collate": true, "column": true,
	"constraint": true, "create": true, "current_catalog": true,
	"current_date": true, "current_role": true, "current_time": true,
	"current_timestamp": true, "current_user": true, "default": true,
	"deferrable": true, "desc": true, "distinct": true, "do": true, "else": true,
	"end": true, "except": true, "false": true, "fetch": true, "for": true,
	"foreign": true, "from": true, "grant": true, "group": true, "having": true,
	"in": true, "initially": true, "intersect": true, "into": true,
	"lateral": true, "leading": true, "limit": true, "localtime": true,
	"localtimestamp": true, "not": true, "null": true, "offset": true, "on": true,
	"only": true, "or": true, "order": true, "placing": true, "primary": true,
	"references": true, "returning": true, "select": true, "session_user": true,
	"some": true, "symmetric": true, "system_user": true, "table": true,
	"then": true, "to": true, "trailing": true, "true": true, "union": true,
	"unique": true, "user": true, "using": true, "variadic": true, "when": true,
	"where": true, "window": true, "with": true,
}

// quoteIdent renders a catalog name: bare when PostgreSQL would read it
// back unchanged, otherwise double-quoted.
func quoteIdent(name string) string {
	bare := name != "" && !reservedWords[name] && (name[0] < '0' || name[0] > '9')
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			bare = false
			break
		}
	}
	if bare {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// qualified is the canonical "schema.name" of a catalog object.
func qualified(schema, name string) string {
	return quoteIdent(schema) + "." + quoteIdent(name)
}

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
	return qualified(schema, name)
}
