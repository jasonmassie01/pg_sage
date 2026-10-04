package executor

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// rewriteTarget names the relation a statement rewrites and reports
// whether it rewrites one at all, from the SQL alone: VACUUM FULL,
// CLUSTER, REINDEX without CONCURRENTLY, and the ALTER TABLE forms that
// rewrite the heap (column type changes, SET LOGGED/UNLOGGED, SET
// TABLESPACE, SET ACCESS METHOD, and an added column that is generated,
// an identity or has a default that is not a plain literal). A rewrite
// with no single relation (VACUUM FULL of a database, REINDEX SCHEMA)
// returns "" and true. Everything else rewrites nothing.
func rewriteTarget(sql string) (string, bool) {
	normalized := strings.TrimSpace(strings.TrimRight(normalizeSQLText(sql), "; "))
	fields := strings.Fields(strings.ToUpper(normalized))
	if len(fields) == 0 {
		return "", false
	}
	switch fields[0] {
	case "VACUUM":
		if vacuumRewrites(fields) {
			return vacuumObject(normalized), true
		}
	case "CLUSTER":
		return clusterObject(normalized), true
	case "REINDEX":
		if !reindexConcurrently(fields) {
			return reindexObject(normalized), true
		}
	case "ALTER":
		if len(fields) > 1 && fields[1] == "TABLE" && alterTableRewrites(normalized) {
			return statementTarget(normalized, "ALTER TABLE"), true
		}
	}
	return "", false
}

// vacuumRewrites reports VACUUM FULL, bare or as an enabled option.
func vacuumRewrites(upperFields []string) bool {
	if len(upperFields) < 2 {
		return false
	}
	if !strings.HasPrefix(upperFields[1], "(") {
		for _, field := range upperFields[1:] {
			switch strings.Trim(field, ",;") {
			case "FULL":
				return true
			case "FREEZE", "VERBOSE", "ANALYZE":
				continue
			}
			return false
		}
		return false
	}
	return optionEnabled(upperFields, "FULL")
}

// reindexConcurrently reports REINDEX ... CONCURRENTLY, as a keyword or an
// enabled option.
func reindexConcurrently(upperFields []string) bool {
	if len(upperFields) > 1 && strings.HasPrefix(upperFields[1], "(") &&
		optionEnabled(upperFields, "CONCURRENTLY") {
		return true
	}
	for _, field := range upperFields {
		if field == "CONCURRENTLY" {
			return true
		}
	}
	return false
}

// optionEnabled reports name in the parenthesized option list that starts
// at upperFields[1], unless it is switched off (FALSE, OFF or 0).
func optionEnabled(upperFields []string, name string) bool {
	joined := strings.Join(upperFields[1:skipOptionList(upperFields, 1)], " ")
	joined = strings.Trim(joined, "() ")
	for _, option := range strings.Split(joined, ",") {
		words := strings.Fields(strings.Trim(option, "() "))
		if len(words) == 0 || words[0] != name {
			continue
		}
		return len(words) == 1 || (words[1] != "FALSE" && words[1] != "OFF" && words[1] != "0")
	}
	return false
}

// clusterObject is the table a CLUSTER names, past its options.
func clusterObject(sql string) string {
	fields := strings.Fields(sql)
	i := skipOptionList(fields, 1)
	if i < len(fields) && strings.EqualFold(fields[i], "VERBOSE") {
		i++
	}
	if i >= len(fields) {
		return ""
	}
	return cleanupIdentifierToken(fields[i])
}

var (
	alterTypePattern    = regexp.MustCompile(`^ALTER (COLUMN )?\S+ (SET DATA )?TYPE\b`)
	rewritingSetPattern = regexp.MustCompile(
		`^SET (LOGGED|UNLOGGED|TABLESPACE\b|ACCESS METHOD\b)`)
	addTableConstraint = regexp.MustCompile(
		`^ADD (CONSTRAINT|PRIMARY|UNIQUE|CHECK|FOREIGN|EXCLUDE)\b`)
	literalDefault = regexp.MustCompile(`^DEFAULT (NULL|TRUE|FALSE|[+-]?[0-9]+(\.[0-9]+)?|'')` +
		`( ?:: ?[A-Z_][A-Z0-9_]*( ?\( ?[0-9]+( ?, ?[0-9]+)? ?\))?(\[\])?)?`)
	columnConstraint = regexp.MustCompile(
		`^(NOT NULL|NULL|CHECK|UNIQUE|PRIMARY KEY|REFERENCES|CONSTRAINT|COLLATE)\b`)
)

// alterTableRewrites reports an ALTER TABLE with a heap-rewriting
// sub-command. String literals are masked first, so a default of
// 'random()' is the literal it is.
func alterTableRewrites(normalized string) bool {
	fields := strings.Fields(maskSQLLiterals(strings.ToUpper(normalized)))
	i := 2
	for i < len(fields) && (fields[i] == "IF" || fields[i] == "EXISTS" || fields[i] == "ONLY") {
		i++
	}
	if i+1 >= len(fields) {
		return false
	}
	for _, sub := range splitTopLevel(strings.Join(fields[i+1:], " ")) {
		if subcommandRewrites(strings.TrimSpace(sub)) {
			return true
		}
	}
	return false
}

func subcommandRewrites(sub string) bool {
	switch {
	case alterTypePattern.MatchString(sub), rewritingSetPattern.MatchString(sub):
		return true
	case !strings.HasPrefix(sub, "ADD ") || addTableConstraint.MatchString(sub):
		return false
	case strings.Contains(sub, " GENERATED ") &&
		(strings.Contains(sub, " STORED") || strings.Contains(sub, " IDENTITY")):
		return true
	}
	at := strings.Index(sub, " DEFAULT ")
	if at < 0 {
		return false
	}
	rest := sub[at+1:]
	literal := literalDefault.FindString(rest)
	if literal == "" {
		return true // a function call or expression: assume it is volatile
	}
	tail := strings.TrimSpace(rest[len(literal):])
	return tail != "" && !columnConstraint.MatchString(tail)
}

// maskSQLLiterals replaces each single-quoted literal with ” and each
// double-quoted identifier with "X", so keywords inside them are not read.
func maskSQLLiterals(sql string) string {
	var out strings.Builder
	var quote rune
	runes := []rune(sql)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if quote == 0 {
			if r == '\'' || r == '"' {
				quote = r
			}
			out.WriteRune(r)
			continue
		}
		if r != quote {
			continue
		}
		if i+1 < len(runes) && runes[i+1] == quote {
			i++ // doubled quote inside the literal
			continue
		}
		if quote == '"' {
			out.WriteRune('X')
		}
		out.WriteRune(r)
		quote = 0
	}
	return out.String()
}

// splitTopLevel splits ALTER TABLE sub-commands at commas outside
// parentheses.
func splitTopLevel(sql string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range sql {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, sql[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, sql[start:])
}

// estimateRowsRewritten estimates the rows sql rewrites from the catalog at
// decision time: the larger of reltuples and the live-tuple count, summed
// over every leaf partition, of the rewritten table (an index's table for
// REINDEX INDEX). A statement that rewrites nothing is 0 without a read; a
// rewrite whose table cannot be resolved is an error, never 0.
func (e *Executor) estimateRowsRewritten(ctx context.Context, sql string) (int64, error) {
	relation, rewrites := rewriteTarget(sql)
	if !rewrites {
		return 0, nil
	}
	if relation == "" {
		return 0, fmt.Errorf("estimate rows rewritten: %q names no single relation", sql)
	}
	if e.pool == nil {
		return 0, fmt.Errorf("estimate rows rewritten: no database pool")
	}
	var found bool
	var rows int64
	err := e.pool.QueryRow(ctx, rowsRewrittenSQL, relation).Scan(&found, &rows)
	if err != nil {
		return 0, fmt.Errorf("estimate rows rewritten for %s: %w", relation, err)
	}
	if !found {
		return 0, fmt.Errorf("estimate rows rewritten: relation %s not found", relation)
	}
	return rows, nil
}

// rowsRewrittenSQL sums the leaves of the rewritten table: every leaf
// partition of a partitioned table (pg_partition_tree), or the table itself
// (pg_partition_tree returns nothing for a table outside a hierarchy).
const rowsRewrittenSQL = `/* pg_sage */
WITH rel AS (
	SELECT to_regclass($1) AS oid
), tbl AS (
	SELECT COALESCE(i.indrelid, rel.oid) AS oid
	  FROM rel LEFT JOIN pg_index i ON i.indexrelid = rel.oid
	 WHERE rel.oid IS NOT NULL
), leaves AS (
	SELECT p.relid AS oid FROM tbl, pg_partition_tree(tbl.oid) p WHERE p.isleaf
	UNION
	SELECT c.oid FROM tbl JOIN pg_class c ON c.oid = tbl.oid
	 WHERE c.relkind IN ('r', 'm', 't')
)
SELECT EXISTS (SELECT 1 FROM tbl),
       COALESCE((SELECT sum(GREATEST(c.reltuples::bigint, COALESCE(s.n_live_tup, 0), 0))
                   FROM leaves l
                   JOIN pg_class c ON c.oid = l.oid
                   LEFT JOIN pg_stat_all_tables s ON s.relid = c.oid), 0)::bigint`
