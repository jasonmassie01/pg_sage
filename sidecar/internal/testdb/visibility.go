package testdb

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Execer runs statements and reads rows: a pool or a connection (VACUUM
// cannot run inside a transaction).
type Execer interface {
	Querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// VacuumAllVisible runs VACUUM (ANALYZE) on table (schema-qualified)
// until its visibility map covers every page, so an index-only scan of it
// fetches no heap rows. VACUUM cannot mark a page all-visible while any
// snapshot of the same database predates the page's rows (an autovacuum
// ANALYZE, another session's open transaction), so one VACUUM leaves a
// test that counts heap fetches to timing. It retries until timeout and
// then names the snapshot holders.
func VacuumAllVisible(ctx context.Context, db Execer, table string,
	timeout time.Duration) error {
	var name string
	if err := db.QueryRow(ctx, "SELECT $1::regclass::text", table).Scan(&name); err != nil {
		return fmt.Errorf("resolve %s: %w", table, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		if _, err := db.Exec(ctx, "VACUUM (ANALYZE) "+name); err != nil {
			return fmt.Errorf("vacuum %s: %w", table, err)
		}
		var visible, pages int
		if err := db.QueryRow(ctx, `SELECT relallvisible, relpages FROM pg_class
			WHERE oid = $1::regclass`, name).Scan(&visible, &pages); err != nil {
			return fmt.Errorf("read visibility of %s: %w", table, err)
		}
		if visible >= pages {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s: %d of %d pages all-visible after %v; snapshot "+
				"holders of this database: %s", table, visible, pages, timeout,
				snapshotHolders(ctx, db))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("vacuum %s: %w", table, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// snapshotHolders lists the sessions of the current database and the
// replication slots that hold back its visibility horizon.
func snapshotHolders(ctx context.Context, db Querier) string {
	var holders []string
	err := db.QueryRow(ctx, `SELECT COALESCE(array_agg(h ORDER BY h), '{}') FROM (
		SELECT format('pid %s (%s, %s, xmin %s)', pid, backend_type,
		              COALESCE(state, '-'), backend_xmin) AS h
		  FROM pg_stat_activity
		 WHERE datname = current_database() AND pid <> pg_backend_pid()
		   AND backend_xmin IS NOT NULL
		UNION ALL
		SELECT format('slot %s (xmin %s)', slot_name, xmin)
		  FROM pg_replication_slots WHERE xmin IS NOT NULL) s`).Scan(&holders)
	if err != nil {
		return fmt.Sprintf("unknown (%v)", err)
	}
	if len(holders) == 0 {
		return "none seen"
	}
	return strings.Join(holders, ", ")
}
