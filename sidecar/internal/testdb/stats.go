package testdb

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
)

// StatementsQuerier is what ResetStatements runs on: a pool, a connection
// or a transaction.
type StatementsQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const statementsSchemaSQL = `SELECT n.nspname FROM pg_catalog.pg_extension e
	JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
	WHERE e.extname = 'pg_stat_statements'`

// ResetStatements clears pg_stat_statements for the current database only
// (the three-argument reset, on PostgreSQL 14 to 18), so other packages
// testing on the same server keep their entries. A zero-argument reset
// empties every database's entries; only a test on testdb.Exclusive may
// do that (the reset lint enforces it). It returns the extension's dealloc
// count, so a caller can tell an eviction from its own reset. The reset
// statement itself is the one entry left behind.
func ResetStatements(ctx context.Context, q StatementsQuerier) (int64, error) {
	if isNilQuerier(q) {
		return 0, errors.New("testdb: reset pg_stat_statements: no querier")
	}
	var schema string
	err := q.QueryRow(ctx, statementsSchemaSQL).Scan(&schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errors.New("testdb: reset statistics: pg_stat_statements is not " +
			"installed in this database (CREATE EXTENSION pg_stat_statements)")
	}
	if err != nil {
		return 0, fmt.Errorf("testdb: find the pg_stat_statements schema: %w", err)
	}
	ns := pgx.Identifier{schema}.Sanitize()
	// One statement, so only one entry survives the reset; a CTE that calls
	// a volatile function always runs.
	sql := `WITH reset AS (SELECT ` + ns + `.pg_stat_statements_reset(0, d.oid, 0)
		FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database())
		SELECT i.dealloc FROM reset, ` + ns + `.pg_stat_statements_info i`
	var dealloc int64
	if err := q.QueryRow(ctx, sql).Scan(&dealloc); err != nil {
		return 0, fmt.Errorf("testdb: reset pg_stat_statements for the current database: %w",
			err)
	}
	return dealloc, nil
}

// isNilQuerier catches a nil interface and a typed nil pointer (a nil
// *pgxpool.Pool), which would panic on the first call.
func isNilQuerier(q StatementsQuerier) bool {
	if q == nil {
		return true
	}
	v := reflect.ValueOf(q)
	return v.Kind() == reflect.Pointer && v.IsNil()
}
