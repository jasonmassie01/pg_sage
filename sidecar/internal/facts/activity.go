package facts

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ActivityReader reads schemas' activity counters (schemaActivityExpr),
// the baselines a cleanup batch re-checks before each drop. A schema
// missing from the result no longer exists.
type ActivityReader interface {
	SchemaActivity(ctx context.Context, schemas []string) (map[string]int64, error)
}

// activityReadTimeout bounds one baseline read on the monitored database.
const activityReadTimeout = 10 * time.Second

// activityBoundsSQL bounds the read's transaction: it is read-only and
// gives up rather than wait on a lock or a slow catalog.
const activityBoundsSQL = `/* pg_sage facts */ SELECT
  pg_catalog.set_config('statement_timeout', '5s', true),
  pg_catalog.set_config('lock_timeout', '1s', true)`

type poolActivity struct{ pool *pgxpool.Pool }

// NewActivityReader reads schema activity from the monitored database in
// one bounded, read-only catalog aggregate.
func NewActivityReader(pool *pgxpool.Pool) ActivityReader {
	return poolActivity{pool: pool}
}

func (r poolActivity) SchemaActivity(ctx context.Context, schemas []string) (
	map[string]int64, error) {
	out := map[string]int64{}
	if len(schemas) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, activityReadTimeout)
	defer cancel()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("read schema activity: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, activityBoundsSQL); err != nil {
		return nil, fmt.Errorf("read schema activity: set timeouts: %w", err)
	}
	rows, err := tx.Query(ctx, schemaActivitySQL, schemas)
	if err != nil {
		return nil, fmt.Errorf("read schema activity of %d schemas: %w", len(schemas), err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, fmt.Errorf("scan schema activity: %w", err)
		}
		out[name] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema activity of %d schemas: %w", len(schemas), err)
	}
	return out, nil
}
