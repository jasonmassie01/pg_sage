package collector

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// statementsSQL qualifies the pg_stat_statements references in sql by the
// extension's schema (cfg.StatementsSchema, from the startup checks), so
// an extension outside the search_path is still read. Empty keeps sql.
func (c *Collector) statementsSQL(sql string) string {
	return qualifyStatements(sql, c.cfg.StatementsSchema)
}

// statementsDB runs db's statements with their pg_stat_statements
// references qualified by the extension's schema: the capability probe,
// the counters and the cached texts read through it. db itself when the
// schema is unknown (the search_path finds the extension).
func (c *Collector) statementsDB(db catalogQuerier) catalogQuerier {
	if c.cfg.StatementsSchema == "" {
		return db
	}
	return statementsQuerier{db: db, schema: c.cfg.StatementsSchema}
}

// statementsQuerier qualifies each statement before db runs it.
type statementsQuerier struct {
	db     catalogQuerier
	schema string
}

func (q statementsQuerier) Query(ctx context.Context, sql string,
	args ...any) (pgx.Rows, error) {
	return q.db.Query(ctx, qualifyStatements(sql, q.schema), args...)
}

func (q statementsQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return q.db.QueryRow(ctx, qualifyStatements(sql, q.schema), args...)
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
