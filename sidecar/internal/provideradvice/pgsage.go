package provideradvice

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Querier reads rows (a pool, a connection or a transaction).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// maxFindings bounds the findings one reconcile compares.
const maxFindings = 1000

// LoadFindings reads pg_sage's open index findings: the indexes it
// proposes (missing_index, missing_fk_index) and drops (unused_index,
// duplicate_index).
func LoadFindings(ctx context.Context, q Querier) ([]Finding, error) {
	rows, err := q.Query(ctx, `SELECT id, category, COALESCE(object_identifier, ''),
		COALESCE(recommended_sql, ''), COALESCE(detail->>'constraint', '')
		FROM sage.findings
		WHERE status = 'open' AND category = ANY($1::text[])
		ORDER BY id LIMIT $2`,
		[]string{"missing_index", "missing_fk_index", "unused_index", "duplicate_index"},
		maxFindings)
	if err != nil {
		return nil, fmt.Errorf("provider advice: read pg_sage's open index findings: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Finding, error) {
		var f Finding
		err := row.Scan(&f.ID, &f.Category, &f.Object, &f.SQL, &f.Constraint)
		return f, err
	})
	if err != nil {
		return nil, fmt.Errorf("provider advice: read pg_sage's open index findings: %w", err)
	}
	return out, nil
}

// DBEvaluator judges provider-only advice on the monitored database with
// bounded read-only catalog reads and pg_sage's HypoPG what-if.
type DBEvaluator struct {
	pool  *pgxpool.Pool
	hypo  *optimizer.HypoPG
	logFn func(string, string, ...any)
}

// NewDBEvaluator returns the evaluator; logFn may be nil.
func NewDBEvaluator(pool *pgxpool.Pool, logFn func(string, string, ...any)) *DBEvaluator {
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	return &DBEvaluator{pool: pool, hypo: optimizer.NewHypoPG(pool, logFn), logFn: logFn}
}

// readOnly runs fn in a read-only transaction bounded by statement and
// lock timeouts.
func (e *DBEvaluator) readOnly(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin read-only catalog read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only: nothing to keep
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '5s'; "+
		"SET LOCAL lock_timeout = '1s'"); err != nil {
		return fmt.Errorf("bound the catalog read: %w", err)
	}
	return fn(tx)
}

const indexFactSQL = `WITH ix AS (SELECT to_regclass($1) AS oid)
SELECT ix.oid IS NOT NULL AND EXISTS (SELECT 1 FROM pg_catalog.pg_index i
                                       WHERE i.indexrelid = ix.oid),
       COALESCE((SELECT string_agg(conname, ', ' ORDER BY conname)
                   FROM pg_catalog.pg_constraint WHERE conindid = ix.oid), ''),
       COALESCE((SELECT idx_scan FROM pg_catalog.pg_stat_all_indexes
                  WHERE indexrelid = ix.oid), 0)
FROM ix`

// IndexFact reads whether the schema-qualified index exists, the
// constraints that depend on it and its scans on this server.
func (e *DBEvaluator) IndexFact(ctx context.Context, index string) (IndexFact, error) {
	if !strings.Contains(index, ".") || strings.ContainsAny(index, ";\x00") {
		return IndexFact{}, fmt.Errorf("index %q is not a schema-qualified name", index)
	}
	var f IndexFact
	err := e.readOnly(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, indexFactSQL, index).Scan(&f.Exists, &f.Constraint, &f.Scans)
	})
	if err != nil {
		return IndexFact{}, fmt.Errorf("read the catalog facts of index %s: %w", index, err)
	}
	return f, nil
}

// errNoTable: the advice names a table this database does not have.
var errNoTable = errors.New("table does not exist in this database")
