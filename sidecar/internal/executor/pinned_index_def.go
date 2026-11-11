package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pinnedCatalogReadTimeout bounds one catalog read under the pinned path.
const pinnedCatalogReadTimeout = 10 * time.Second

// queryRowPinned runs one catalog read in a read-only transaction whose
// search_path is the executor's. pg_get_indexdef qualifies every object it
// cannot see on the session's path, so text rendered here re-creates the
// same index in a pinned DDL session; text rendered on the application's
// path leaves "'a'::my_enum" or a function outside pg_catalog bare, and
// those do not resolve under the pin (only a bare extension operator class
// is named at run time, by qualifyIndexOpclasses).
func queryRowPinned(
	ctx context.Context, pool *pgxpool.Pool, sql string, args []any, dest ...any,
) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin pinned catalog read: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if err := setLocalSessionScope(ctx, tx, pinnedCatalogReadTimeout, ddlOpts{}); err != nil {
		return err
	}
	return tx.QueryRow(ctx, sql, args...).Scan(dest...)
}

// pinnedIndexDefinition renders a schema-qualified index's definition as a
// pinned DDL session resolves it; "" when no such index exists.
func pinnedIndexDefinition(ctx context.Context, pool *pgxpool.Pool, index string) (string, error) {
	var def string
	err := queryRowPinned(ctx, pool, `/* pg_sage executor */ SELECT COALESCE(
		pg_catalog.pg_get_indexdef(pg_catalog.to_regclass($1)), '')`, []any{index}, &def)
	if err != nil {
		return "", fmt.Errorf("render index %s under the pinned search_path: %w", index, err)
	}
	return def, nil
}

// pinDropRollback replaces a DROP INDEX's re-create rollback, captured on
// the application's path, with the live index rendered under the pinned
// path, so the rollback still runs in a pinned session. Other statements
// and rollbacks pass through; without the index the drop decides alone.
func (e *Executor) pinDropRollback(ctx context.Context, sql, rollbackSQL string) (string, error) {
	upper := strings.ToUpper(normalizeSQLText(sql))
	rollback := strings.ToUpper(normalizeSQLText(rollbackSQL))
	if !strings.HasPrefix(upper, "DROP INDEX ") || (!strings.HasPrefix(rollback, "CREATE INDEX ") &&
		!strings.HasPrefix(rollback, "CREATE UNIQUE INDEX ")) {
		return rollbackSQL, nil
	}
	target := statementTarget(normalizeSQLText(sql), "DROP INDEX")
	def, err := pinnedIndexDefinition(ctx, e.pool, target)
	if err != nil {
		return "", fmt.Errorf("capture the rollback for %q: %w", sql, err)
	}
	if def == "" {
		return rollbackSQL, nil
	}
	return def + ";", nil
}

// manualRollbackSQL checks an operator action's rollback and returns the
// one to record: derived for extended statistics, re-rendered under the
// pinned path for an index drop.
func (e *Executor) manualRollbackSQL(ctx context.Context, sql, rollbackSQL string) (string, error) {
	if err := checkIndexRollback(sql, rollbackSQL); err != nil {
		return "", err
	}
	rollbackSQL, err := statisticsRollback(sql, rollbackSQL)
	if err != nil {
		return "", err
	}
	return e.pinDropRollback(ctx, sql, rollbackSQL)
}
