package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// dropFailedCreateIndexRemnant drops only an INVALID index carrying exactly
// the name the approved CREATE INDEX will create (the remnant of an earlier
// failed CONCURRENTLY build). Other invalid indexes on the table are never
// touched: they are not part of the approved SQL.
func (e *Executor) dropFailedCreateIndexRemnant(
	ctx context.Context, sql string, timeout time.Duration, opts ...DDLOption,
) error {
	schemaName, tableName, cols, ok := parseCreateIndexTarget(sql)
	name := createIndexIdentifier(sql)
	if name == "" && ok {
		name = defaultIndexName(tableName, cols)
	}
	if !ok || name == "" {
		return nil
	}
	if schemaName == "" {
		schemaName = "public"
	}
	qualified := pgx.Identifier{schemaName, name}.Sanitize()
	var invalid bool
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (
		SELECT 1 FROM pg_index i WHERE i.indexrelid = to_regclass($1)
		   AND NOT i.indisvalid)`, qualified).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("check failed index remnant: %w", err)
	}
	if !invalid {
		return nil
	}
	e.logFn("executor", "dropping invalid remnant %s before re-creating it", qualified)
	return ExecConcurrently(ctx, e.pool,
		"DROP INDEX CONCURRENTLY IF EXISTS "+qualified, timeout, opts...)
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

func (e *Executor) createIndexCoverageExists(
	ctx context.Context,
	sql string,
) (bool, error) {
	schemaName, tableName, cols, ok := parseCreateIndexTarget(sql)
	if !ok || len(cols) == 0 {
		return false, nil
	}
	if schemaName == "" {
		schemaName = "public"
	}

	var one int
	err := e.pool.QueryRow(ctx,
		`/* pg_sage */ WITH indexed AS (
		    SELECT i.indexrelid,
		           array_agg(a.attname::text ORDER BY ord.n) AS cols
		      FROM pg_index i
		      JOIN pg_class tbl ON tbl.oid = i.indrelid
		      JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
		      JOIN unnest(i.indkey) WITH ORDINALITY AS ord(attnum, n)
		           ON ord.attnum > 0
		      JOIN pg_attribute a
		           ON a.attrelid = tbl.oid
		          AND a.attnum = ord.attnum
		     WHERE ns.nspname = $1
		       AND tbl.relname = $2
		       AND i.indisvalid
		       AND i.indisready
		       AND i.indpred IS NULL
		     GROUP BY i.indexrelid
		)
		SELECT 1
		  FROM indexed
		 WHERE cols[1:cardinality($3::text[])] = $3::text[]
		 LIMIT 1`,
		schemaName, tableName, cols,
	).Scan(&one)
	if err == nil {
		return true, nil
	}
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return false, err
}

func parseCreateIndexTarget(sql string) (string, string, []string, bool) {
	compact := strings.Join(strings.Fields(strings.TrimSuffix(
		strings.TrimSpace(sql), ";")), " ")
	upper := strings.ToUpper(compact)
	onIdx := strings.Index(upper, " ON ")
	if onIdx < 0 {
		return "", "", nil, false
	}
	afterOn := strings.TrimSpace(compact[onIdx+4:])
	if strings.HasPrefix(strings.ToUpper(afterOn), "ONLY ") {
		afterOn = strings.TrimSpace(afterOn[5:])
	}
	openParen := strings.Index(afterOn, "(")
	if openParen < 0 {
		return "", "", nil, false
	}
	tableSpec := strings.TrimSpace(afterOn[:openParen])
	if usingIdx := strings.Index(strings.ToUpper(tableSpec), " USING "); usingIdx >= 0 {
		tableSpec = strings.TrimSpace(tableSpec[:usingIdx])
	}
	tableSpec = strings.TrimSpace(tableSpec)
	if tableSpec == "" {
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

	parts := splitQualifiedIdentifier(tableSpec)
	if len(parts) == 1 {
		return "", parts[0], cols, true
	}
	if len(parts) == 2 {
		return parts[0], parts[1], cols, true
	}
	return "", "", nil, false
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
