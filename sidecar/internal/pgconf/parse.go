// Package pgconf is the single source of truth for the PostgreSQL
// configuration changes pg_sage may propose or execute: the parsers for
// ALTER SYSTEM and ALTER TABLE ... SET/RESET (storage parameters), the
// restart-required list, the documented safe ranges, and the GUC and
// reloption allowlists. The advisor, the executor and the SQL validators
// all read these, so they can no longer disagree (G-P0-1).
package pgconf

import (
	"strings"
)

// SystemStmt is one ALTER SYSTEM SET/RESET of a single setting.
type SystemStmt struct {
	Name  string // lower-cased, unquoted GUC name
	Value string // unquoted value ("" for RESET)
	Reset bool
}

// ParseAlterSystem parses "ALTER SYSTEM SET name {=|TO} value" or
// "ALTER SYSTEM RESET name". RESET ALL and malformed statements are not
// single-setting changes and return ok=false.
func ParseAlterSystem(sql string) (SystemStmt, bool) {
	s := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(sql), ";"))
	rest, ok := cutKeywords(s, "ALTER", "SYSTEM")
	if !ok {
		return SystemStmt{}, false
	}
	if r, ok := cutKeywords(rest, "RESET"); ok {
		name := normalizeName(r)
		if name == "" || name == "all" || strings.ContainsAny(name, " \t=") {
			return SystemStmt{}, false
		}
		return SystemStmt{Name: name, Reset: true}, true
	}
	if r, ok := cutKeywords(rest, "SET"); ok {
		return parseSetClause(r)
	}
	return SystemStmt{}, false
}

func parseSetClause(rest string) (SystemStmt, bool) {
	name, value, found := strings.Cut(rest, "=")
	if !found {
		lower := strings.ToLower(rest)
		i := strings.Index(lower, " to ")
		if i < 0 {
			return SystemStmt{}, false
		}
		name, value = rest[:i], rest[i+len(" to "):]
	}
	name = normalizeName(name)
	value = Unquote(strings.TrimSpace(value))
	if name == "" || strings.ContainsAny(name, " \t") || value == "" {
		return SystemStmt{}, false
	}
	return SystemStmt{Name: name, Value: value}, true
}

func normalizeName(s string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(s), `"`))
}

// Unquote removes one pair of surrounding single quotes and undoubles
// embedded quotes; other values are returned trimmed.
func Unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	return v
}

// Reloption is one storage parameter of an ALTER TABLE SET/RESET list.
type Reloption struct {
	Key   string // lower-cased, "toast." namespace kept
	Value string // unquoted value; "" in a RESET list
}

// TableStmt is "ALTER TABLE [IF EXISTS] [ONLY] <table> SET|RESET (...)".
type TableStmt struct {
	Table   string // identifier as written (quotes kept)
	Options []Reloption
	Reset   bool
}

// ParseAlterTableReloptions parses an ALTER TABLE whose only subcommand is
// SET (...) or RESET (...) of storage parameters.
func ParseAlterTableReloptions(sql string) (TableStmt, bool) {
	s := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(sql), ";"))
	rest, ok := cutKeywords(s, "ALTER", "TABLE")
	if !ok {
		return TableStmt{}, false
	}
	rest, _ = cutKeywords(rest, "IF", "EXISTS")
	rest, _ = cutKeywords(rest, "ONLY")
	table, rest := splitIdentifier(rest)
	if table == "" {
		return TableStmt{}, false
	}
	stmt := TableStmt{Table: table}
	if r, ok := cutKeywords(rest, "RESET"); ok {
		stmt.Reset, rest = true, r
	} else if r, ok := cutKeywords(rest, "SET"); ok {
		rest = r
	} else {
		return TableStmt{}, false
	}
	opts, ok := parseOptionList(rest, stmt.Reset)
	if !ok {
		return TableStmt{}, false
	}
	stmt.Options = opts
	return stmt, true
}

// cutKeywords strips the given case-insensitive keywords (each followed
// by whitespace or "(") from the front of s.
func cutKeywords(s string, words ...string) (string, bool) {
	rest := strings.TrimSpace(s)
	for _, w := range words {
		if len(rest) < len(w) || !strings.EqualFold(rest[:len(w)], w) {
			return s, false
		}
		after := rest[len(w):]
		if after != "" && after[0] != ' ' && after[0] != '\t' && after[0] != '(' {
			return s, false
		}
		rest = strings.TrimSpace(after)
	}
	return rest, true
}

// splitIdentifier returns a possibly quoted, possibly qualified name and
// what follows it.
func splitIdentifier(s string) (string, string) {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inQuote = !inQuote
		case !inQuote && (c == ' ' || c == '\t' || c == '('):
			return s[:i], strings.TrimSpace(s[i:])
		}
	}
	return "", ""
}

// parseOptionList parses "(k = v, ...)" (or "(k, ...)" for RESET); nothing
// may follow the closing parenthesis.
func parseOptionList(s string, reset bool) ([]Reloption, bool) {
	s = strings.TrimSpace(s)
	end := closingParen(s)
	if !strings.HasPrefix(s, "(") || end < 0 || strings.TrimSpace(s[end+1:]) != "" {
		return nil, false
	}
	var opts []Reloption
	for _, part := range splitOutsideQuotes(s[1:end], ',') {
		opt, ok := parseOption(part, reset)
		if !ok {
			return nil, false
		}
		opts = append(opts, opt)
	}
	return opts, len(opts) > 0
}

func parseOption(part string, reset bool) (Reloption, bool) {
	kv := splitOutsideQuotes(part, '=')
	key := normalizeName(kv[0])
	if key == "" || strings.ContainsAny(key, " \t") || len(kv) > 2 {
		return Reloption{}, false
	}
	switch {
	case reset && len(kv) == 1:
		return Reloption{Key: key}, true
	case reset:
		return Reloption{}, false
	case len(kv) == 1: // a bare boolean option means true
		return Reloption{Key: key, Value: "true"}, true
	}
	value := Unquote(kv[1])
	return Reloption{Key: key, Value: value}, value != ""
}

// closingParen returns the index of the parenthesis closing s[0], outside
// single- and double-quoted text, or -1.
func closingParen(s string) int {
	depth, quote := 0, byte(0)
	for i := 0; i < len(s); i++ {
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

// splitOutsideQuotes splits s at sep where it is not inside quotes.
func splitOutsideQuotes(s string, sep byte) []string {
	var parts []string
	start, quote := 0, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == sep:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// ReloptionBaseKey splits a reloption key into its name and whether it
// targets the TOAST table ("toast." namespace).
func ReloptionBaseKey(key string) (base string, toast bool) {
	k := strings.ToLower(strings.TrimSpace(key))
	if strings.HasPrefix(k, "toast.") {
		return strings.TrimPrefix(k, "toast."), true
	}
	return k, false
}
