package optimizer

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// coldStartSQL counts at most $1 snapshot rows: the check only needs to
// know whether minSnapshots exist, so it never scans the whole history
// (it used to COUNT(*) all of sage.snapshots on every run; Phase 0 item 8).
const coldStartSQL = `/* pg_sage */ SELECT count(*) FROM
  (SELECT 1 FROM sage.snapshots LIMIT $1) s`

// CheckColdStart returns true if there are fewer than minSnapshots
// in sage.snapshots, indicating insufficient data for optimization.
// A non-positive minSnapshots disables the check. On error it reports
// cold (fail closed) with the error.
func CheckColdStart(
	ctx context.Context,
	pool *pgxpool.Pool,
	minSnapshots int,
) (bool, error) {
	if minSnapshots <= 0 {
		return false, nil
	}
	var count int
	if err := pool.QueryRow(ctx, coldStartSQL, minSnapshots).Scan(&count); err != nil {
		return true, fmt.Errorf("cold-start snapshot check: %w", err)
	}
	return count < minSnapshots, nil
}
