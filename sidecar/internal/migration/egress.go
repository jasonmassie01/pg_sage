package migration

import (
	"fmt"
	"strings"
)

// redactedLiteral replaces every string literal in observed DDL. Literals
// are the carrier for credentials (PASSWORD '...', CONNECTION '...',
// OPTIONS (password '...')) and for business data, so pg_sage never
// persists or ships them anywhere (G7-B01).
const redactedLiteral = "'***'"

// sanitizeDDL strips comments and replaces every string literal
// ('...', E'...', U&'...', $tag$...$tag$) with '***'. Quoted identifiers
// are preserved. The result is safe to log, persist, or send to an LLM
// as far as literals are concerned; callers must still gate egress with
// llmEgressAllowed because identifiers can be sensitive in context.
func sanitizeDDL(sql string) string {
	clean, _ := scanDDL(sql)
	return clean
}

// scanDDL is sanitizeDDL that also reports whether every literal, quoted
// identifier, dollar-quoted body and block comment was closed. Text that
// ends inside one is redacted to its end, so it can hide statements.
func scanDDL(sql string) (string, bool) {
	var b strings.Builder
	b.Grow(len(sql))
	closed := true
	for i := 0; i < len(sql); {
		next, token, ok := scanToken(sql, i)
		b.WriteString(token)
		closed = closed && ok
		i = next
	}
	return strings.TrimSpace(b.String()), closed
}

// scanToken consumes one lexical unit starting at i and returns the
// index after it, its sanitized replacement, and whether it was closed.
func scanToken(sql string, i int) (int, string, bool) {
	c := sql[i]
	switch {
	case c == '-' && strings.HasPrefix(sql[i:], "--"):
		end := strings.IndexByte(sql[i:], '\n')
		if end < 0 {
			return len(sql), " ", true
		}
		return i + end, " ", true
	case c == '/' && strings.HasPrefix(sql[i:], "/*"):
		end, ok := skipBlockComment(sql, i)
		return end, " ", ok
	case c == '\'':
		end, ok := skipQuoted(sql, i, isEscapeString(sql, i))
		return end, redactedLiteral, ok
	case c == '"':
		end, ok := skipIdentifier(sql, i)
		return end, sql[i:end], ok
	case c == '$':
		if end, isQuote, ok := skipDollarQuoted(sql, i); isQuote {
			return end, redactedLiteral, ok
		}
	}
	return i + 1, sql[i : i+1], true
}

// skipBlockComment handles PostgreSQL's nested /* */ comments. It returns
// the index after the comment and whether the comment was closed.
func skipBlockComment(sql string, i int) (int, bool) {
	depth := 0
	for j := i; j < len(sql)-1; j++ {
		switch {
		case sql[j] == '/' && sql[j+1] == '*':
			depth++
			j++
		case sql[j] == '*' && sql[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				return j + 1, true
			}
		}
	}
	return len(sql), false
}

// isEscapeString reports whether the quote at i opens an E'...' string,
// where backslash escapes the next character.
func isEscapeString(sql string, i int) bool {
	if i == 0 || (sql[i-1] != 'E' && sql[i-1] != 'e') {
		return false
	}
	return i < 2 || !isIdentByte(sql[i-2])
}

// skipQuoted returns the index after the literal opened at i and whether
// its closing quote was found.
func skipQuoted(sql string, i int, backslashEscapes bool) (int, bool) {
	for j := i + 1; j < len(sql); j++ {
		switch {
		case backslashEscapes && sql[j] == '\\':
			j++
		case sql[j] == '\'':
			if j+1 < len(sql) && sql[j+1] == '\'' {
				j++
				continue
			}
			return j + 1, true
		}
	}
	return len(sql), false
}

// skipIdentifier returns the index after the quoted identifier opened at
// i and whether its closing quote was found.
func skipIdentifier(sql string, i int) (int, bool) {
	for j := i + 1; j < len(sql); j++ {
		if sql[j] == '"' {
			if j+1 < len(sql) && sql[j+1] == '"' {
				j++
				continue
			}
			return j + 1, true
		}
	}
	return len(sql), false
}

// skipDollarQuoted recognizes $$...$$ and $tag$...$tag$. Positional
// parameters ($1) and identifiers containing $ are not quotes. It returns
// the index after the body, whether i opens one, and whether it closed.
func skipDollarQuoted(sql string, i int) (int, bool, bool) {
	if i > 0 && isIdentByte(sql[i-1]) {
		return 0, false, false
	}
	k := i + 1
	if k < len(sql) && sql[k] >= '0' && sql[k] <= '9' {
		return 0, false, false
	}
	for k < len(sql) && isIdentByte(sql[k]) && sql[k] != '$' {
		k++
	}
	if k >= len(sql) || sql[k] != '$' {
		return 0, false, false
	}
	tag := sql[i : k+1]
	closing := strings.Index(sql[k+1:], tag)
	if closing < 0 {
		return len(sql), true, false
	}
	return k + 1 + closing + len(tag), true, true
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c >= 0x80
}

// splitStatements splits sanitized SQL on top-level semicolons. Input
// must come from sanitizeDDL, so literals and comments are gone and
// only quoted identifiers can contain a ';'.
func splitStatements(clean string) []string {
	var out []string
	start := 0
	for i := 0; i < len(clean); i++ {
		switch clean[i] {
		case '"':
			end, _ := skipIdentifier(clean, i)
			i = end - 1
		case ';':
			out = appendStatement(out, clean[start:i])
			start = i + 1
		}
	}
	return appendStatement(out, clean[start:])
}

func appendStatement(out []string, stmt string) []string {
	if s := strings.TrimSpace(stmt); s != "" {
		return append(out, s)
	}
	return out
}

// llmTablePrefixes are the only statement shapes that may be sent to
// the external LLM: table/index-level DDL and maintenance commands.
var llmTablePrefixes = []string{
	"ALTER TABLE ", "ALTER INDEX ", "ALTER MATERIALIZED VIEW ",
	"DROP TABLE ", "DROP INDEX ", "CREATE INDEX ", "CREATE UNIQUE INDEX ",
	"REINDEX ", "CLUSTER", "VACUUM", "REFRESH MATERIALIZED VIEW ",
}

// llmCredentialMarkers deny egress even for table DDL; they indicate a
// statement shape that can carry connection strings or passwords.
var llmCredentialMarkers = []string{
	"PASSWORD", "USER MAPPING", "SUBSCRIPTION", "CONNECTION",
	"CONNINFO", "SERVER ", "SECRET",
}

// llmProvablySafeMarkers identify DDL that is already the safe form;
// asking the LLM about it only costs tokens and leaks schema (G7-B27).
var llmProvablySafeMarkers = []string{
	" CONCURRENTLY", "NOT VALID", "VALIDATE CONSTRAINT",
}

// llmEgressAllowed reports whether sanitized DDL may leave the host.
// Unclassifiable statements default to NOT being sent (fail closed).
func llmEgressAllowed(clean string) bool {
	stmts := splitStatements(clean)
	if len(stmts) != 1 {
		return false
	}
	upper := strings.ToUpper(collapseWhitespace(stmts[0])) + " "
	if !hasAnyPrefix(upper, llmTablePrefixes) {
		return false
	}
	if containsAny(upper, llmCredentialMarkers) {
		return false
	}
	return !containsAny(upper, llmProvablySafeMarkers)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// formatPGVersion renders server_version_num (160004) as "16.4";
// small values are treated as a bare major version.
func formatPGVersion(v int) string {
	if v >= 100000 {
		return fmt.Sprintf("%d.%d", v/10000, v%10000)
	}
	return fmt.Sprintf("%d", v)
}
