package collector

import (
	"strings"

	"github.com/jackc/pgx/v5"
)

// statementsSQL qualifies the pg_stat_statements references in sql by the
// extension's schema (cfg.StatementsSchema, from the startup checks), so
// an extension outside the search_path is still read. Empty keeps sql.
func (c *Collector) statementsSQL(sql string) string {
	return qualifyStatements(sql, c.cfg.StatementsSchema)
}

// qualifyStatements rewrites the exact pg_stat_statements references the
// collector's statements use: the view and its info view in FROM, the
// function calls, and the to_regclass probes (as a string literal). A
// setting name such as pg_stat_statements.track_utility is left alone.
func qualifyStatements(sql, schema string) string {
	if schema == "" {
		return sql
	}
	ns := pgx.Identifier{schema}.Sanitize() + "."
	regclass := func(name string) string {
		return "to_regclass('" + strings.ReplaceAll(ns+name, "'", "''") + "')"
	}
	// strings.Replacer tries the old strings in argument order at each
	// position, so the longer _info forms win over the bare name, and a
	// replaced span is never rewritten twice.
	return strings.NewReplacer(
		"to_regclass('pg_stat_statements_info')", regclass("pg_stat_statements_info"),
		"to_regclass('pg_stat_statements')", regclass("pg_stat_statements"),
		"FROM pg_stat_statements_info", "FROM "+ns+"pg_stat_statements_info",
		"FROM pg_stat_statements", "FROM "+ns+"pg_stat_statements",
		"pg_stat_statements(", ns+"pg_stat_statements(",
	).Replace(sql)
}
