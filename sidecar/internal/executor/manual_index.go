package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// refuseUnboundRemnant refuses an approved CREATE INDEX whose name is
// held by an INVALID index. pg_sage's own failed builds are cleaned up by
// the remnant handler, bound to the OID they registered; an INVALID index
// it cannot bind may be another session's build still running, so it is
// never dropped by name here (executor-05).
func (e *Executor) refuseUnboundRemnant(ctx context.Context, sql string) error {
	schemaName, name, ok := createIndexName(sql)
	if !ok {
		return nil
	}
	qualified := pgx.Identifier{schemaName, name}.Sanitize()
	var invalid bool
	err := e.pool.QueryRow(ctx, `/* pg_sage executor */ SELECT EXISTS (
		SELECT 1 FROM pg_index i WHERE i.indexrelid = to_regclass($1)
		   AND NOT i.indisvalid)`, qualified).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("check for an INVALID index named %s: %w", qualified, err)
	}
	if invalid {
		return fmt.Errorf("index %s already exists and is INVALID (a failed build's "+
			"remnant, or a build still running); confirm no build is running, drop it with "+
			"DROP INDEX CONCURRENTLY IF EXISTS %s, then approve again", qualified, qualified)
	}
	return nil
}

// createIndexName is the schema (public when unqualified) and name of the
// index a CREATE INDEX statement creates, PostgreSQL's generated name for an
// unnamed one; ok is false when the statement does not tell.
func createIndexName(sql string) (string, string, bool) {
	schemaName, tableName, cols, ok := parseCreateIndexTarget(sql)
	name := createIndexIdentifier(sql)
	if name == "" && ok {
		name = defaultIndexName(tableName, cols)
	}
	if !ok || name == "" {
		return "", "", false
	}
	if schemaName == "" {
		schemaName = "public"
	}
	return schemaName, name, true
}

// defaultIndexName mirrors PostgreSQL's generated name for an unnamed
// CREATE INDEX (<table>_<columns>_idx) when it fits in NAMEDATALEN; longer
// names are truncated by the server and are not guessed here.
func defaultIndexName(table string, cols []string) string {
	name := table + "_" + strings.Join(cols, "_") + "_idx"
	if len(name) > 63 {
		return ""
	}
	return name
}

// createIndexIdentifier returns the index name of a CREATE INDEX statement
// as PostgreSQL stores it: quoted names keep their case, bare names fold to
// lower case.
func createIndexIdentifier(sql string) string {
	fields := strings.Fields(normalizeSQLText(sql))
	i := 0
	for i < len(fields) && containsFold(
		[]string{"CREATE", "UNIQUE", "INDEX", "CONCURRENTLY"}, fields[i]) {
		i++
	}
	if i+2 < len(fields) && strings.EqualFold(fields[i], "IF") &&
		strings.EqualFold(fields[i+1], "NOT") && strings.EqualFold(fields[i+2], "EXISTS") {
		i += 3
	}
	if i >= len(fields) || strings.EqualFold(fields[i], "ON") {
		return ""
	}
	token := strings.TrimSpace(fields[i])
	if strings.HasPrefix(token, `"`) {
		return unquoteIdentifier(token)
	}
	return strings.ToLower(token)
}

func parseCreateIndexTarget(sql string) (string, string, []string, bool) {
	schemaName, tableName, afterOn, openParen, ok := createIndexOn(sql)
	if !ok {
		return "", "", nil, false
	}
	closeParen := matchingCloseParen(afterOn, openParen)
	if closeParen < 0 {
		return "", "", nil, false
	}
	cols := normalizeIndexColumns(afterOn[openParen+1 : closeParen])
	if len(cols) == 0 {
		return "", "", nil, false
	}
	return schemaName, tableName, cols, true
}

// createIndexOn is the schema ("" when unqualified) and table of a
// CREATE INDEX, with the text after ON and where its key list opens; it
// reads expression indexes too.
func createIndexOn(sql string) (string, string, string, int, bool) {
	compact := strings.Join(strings.Fields(strings.TrimSuffix(
		strings.TrimSpace(sql), ";")), " ")
	upper := strings.ToUpper(compact)
	onIdx := strings.Index(upper, " ON ")
	if onIdx < 0 {
		return "", "", "", 0, false
	}
	afterOn := strings.TrimSpace(compact[onIdx+4:])
	if strings.HasPrefix(strings.ToUpper(afterOn), "ONLY ") {
		afterOn = strings.TrimSpace(afterOn[5:])
	}
	openParen := strings.Index(afterOn, "(")
	if openParen < 0 {
		return "", "", "", 0, false
	}
	tableSpec := strings.TrimSpace(afterOn[:openParen])
	if usingIdx := strings.Index(strings.ToUpper(tableSpec), " USING "); usingIdx >= 0 {
		tableSpec = strings.TrimSpace(tableSpec[:usingIdx])
	}
	parts := splitQualifiedIdentifier(strings.TrimSpace(tableSpec))
	switch {
	case tableSpec == "" || len(parts) > 2:
		return "", "", "", 0, false
	case len(parts) == 1:
		return "", parts[0], afterOn, openParen, true
	}
	return parts[0], parts[1], afterOn, openParen, true
}

func matchingCloseParen(s string, open int) int {
	depth := 0
	inQuote := false
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case '(':
			if !inQuote {
				depth++
			}
		case ')':
			if !inQuote {
				depth--
				if depth == 0 {
					return i
				}
			}
		}
	}
	return -1
}

func normalizeIndexColumns(s string) []string {
	parts := splitTopLevelCSV(s)
	cols := make([]string, 0, len(parts))
	for _, part := range parts {
		col := strings.TrimSpace(part)
		if col == "" || strings.ContainsAny(col, "()") {
			continue
		}
		fields := strings.Fields(col)
		if len(fields) == 0 {
			continue
		}
		cols = append(cols, unquoteIdentifier(fields[0]))
	}
	return cols
}

func splitTopLevelCSV(s string) []string {
	var parts []string
	depth := 0
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case '(':
			if !inQuote {
				depth++
			}
		case ')':
			if !inQuote {
				depth--
			}
		case ',':
			if !inQuote && depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func splitQualifiedIdentifier(s string) []string {
	var parts []string
	inQuote := false
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inQuote = !inQuote
		case '.':
			if !inQuote {
				parts = append(parts, unquoteIdentifier(s[start:i]))
				start = i + 1
			}
		}
	}
	parts = append(parts, unquoteIdentifier(s[start:]))
	return parts
}

func unquoteIdentifier(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}
