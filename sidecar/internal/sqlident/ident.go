// Package sqlident renders PostgreSQL identifiers for SQL that people read
// and paste: a name stays bare only when the server reads it back
// unchanged in every grammar position, and is double-quoted otherwise.
// The rule matches the server's own quote_ident.
package sqlident

import "strings"

//go:generate go run gen_keywords.go

// Ident quotes name unless it is plain lower case (a-z, 0-9 and _, not
// starting with a digit) and not a reserved (R), type/function-name (T) or
// column-name (C) key word. Quoting a C key word is never wrong, and it
// keeps one rule for every position a name appears in.
func Ident(name string) string {
	if bare(name) {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Qualified is schema.name, each part quoted when needed.
func Qualified(schema, name string) string {
	return Ident(schema) + "." + Ident(name)
}

// Role is a role name for GRANT, REVOKE and ALTER ROLE. Role names follow
// the identifier rules, so the statement names exactly that role and a
// key-word role such as "user" still parses.
func Role(name string) string { return Ident(name) }

func bare(name string) bool {
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	_, keyword := keywords[name]
	return !keyword
}
