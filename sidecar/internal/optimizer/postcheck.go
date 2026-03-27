package optimizer

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CheckIndexValid verifies that a newly created index is valid
// by checking pg_index.indisvalid. Retries briefly to allow for
// catalog propagation delay after CREATE INDEX CONCURRENTLY.
func CheckIndexValid(
	ctx context.Context,
	pool *pgxpool.Pool,
	indexName string,
) (bool, error) {
	const maxAttempts = 3
	const retryDelay = 500 * time.Millisecond

	for attempt := range maxAttempts {
		var valid bool
		err := pool.QueryRow(ctx,
			`SELECT i.indisvalid
			 FROM pg_index i
			 JOIN pg_class c ON c.oid = i.indexrelid
			 WHERE c.relname = $1`,
			indexName,
		).Scan(&valid)
		if err == nil {
			return valid, nil
		}
		// Index not yet visible in catalog — retry after delay.
		if attempt < maxAttempts-1 {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(retryDelay):
			}
		} else {
			return false, err
		}
	}
	return false, nil
}
