package perfgate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// allVisibleWait bounds how long AnalyzeSage waits for the seeded history
// to become all-visible: an autovacuum ANALYZE of the largest seeded table
// on a slow CI runner, not an open-ended transaction.
const allVisibleWait = 2 * time.Minute

// awaitAllVisible vacuums the sage tables VACUUM left partly unmarked
// until every page is all-visible, as on a long-running deployment.
// VACUUM marks a page only once no snapshot in the database can still see
// its rows as in progress; a snapshot taken before the seed's last insert
// (an autovacuum worker analyzing a large table) holds the newest rows
// back until it ends. It fails, naming the tables, after within.
func awaitAllVisible(ctx context.Context, pool *pgxpool.Pool, within time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		tables, err := partlyVisible(ctx, pool)
		if err != nil {
			return err
		}
		if len(tables) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("perfgate: %s not all-visible after %v: a snapshot older "+
				"than their rows is still held in the database", strings.Join(tables, ", "),
				within)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("perfgate: wait for all-visible tables: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
		for _, table := range tables {
			if _, err := pool.Exec(ctx, "VACUUM "+table); err != nil {
				return fmt.Errorf("perfgate: vacuum %s: %w", table, err)
			}
		}
	}
}

// partlyVisible lists the sage tables with pages not marked all-visible.
func partlyVisible(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `SELECT pg_catalog.format('%I.%I', n.nspname, c.relname)
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'sage' AND c.relkind IN ('r', 'm')
		  AND c.relallvisible < c.relpages
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("perfgate: read all-visible pages: %w", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("perfgate: read all-visible pages: %w", err)
	}
	return tables, nil
}
