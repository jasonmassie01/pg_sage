package perfgate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// MoveHistoryToStore moves the seeded snapshots and query store from the
// monitored database into the meta database's history store as database
// id, with the real migration (copy, then cleanup of the source), then
// copies them once for each of others: the store then holds as many other
// databases' rows as this one's, and a read that does not find its own
// rows by index scans them all. A copied delta of another database keeps
// its base in id's rows: nothing reads other databases' documents.
func MoveHistoryToStore(ctx context.Context, monitored, meta *pgxpool.Pool, id int,
	others []int) error {
	dst, err := histstore.NewMeta(meta, id)
	if err != nil {
		return fmt.Errorf("perfgate: history store: %w", err)
	}
	src := histstore.NewMonitored(monitored)
	if _, err := histstore.Migrate(ctx, src, dst, histstore.MigrateOptions{
		BatchRows: 2000}); err != nil {
		return fmt.Errorf("perfgate: migrate history: %w", err)
	}
	if _, err := histstore.Cleanup(ctx, src, dst); err != nil {
		return fmt.Errorf("perfgate: clean up migrated history: %w", err)
	}
	for _, other := range others {
		for _, stmt := range []string{
			`INSERT INTO sage.snapshots (collected_at, category, data, base_id, database_id)
			 SELECT collected_at, category, data, base_id, $2 FROM sage.snapshots
			 WHERE database_id = $1`,
			`INSERT INTO sage.query_store (captured_at, queryid, calls, total_exec_time,
			     mean_exec_time, rows, plan_hash, stats_epoch, database_id)
			 SELECT captured_at, queryid, calls, total_exec_time, mean_exec_time, rows,
			     plan_hash, stats_epoch, $2 FROM sage.query_store WHERE database_id = $1`,
		} {
			if _, err := meta.Exec(ctx, "/* "+HarnessTag+" */ "+stmt, id, other); err != nil {
				return fmt.Errorf("perfgate: copy history for database %d: %w", other, err)
			}
		}
	}
	return nil
}
