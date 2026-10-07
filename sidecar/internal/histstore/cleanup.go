package histstore

import (
	"context"
	"fmt"
	"time"
)

// CleanupReport is the source rows a cleanup removed per table.
type CleanupReport struct {
	Removed map[string]int64
}

// cleanupDayBatch bounds one delete of a shared table (a meta store's
// rows of one database): a day of rows at a time.
const cleanupDayBatch = 24 * time.Hour

// Cleanup removes the source's history once Migrate copied all of it to
// dst: a monitored database's tables are truncated; a meta store's (or the
// meta database's own) rows are deleted a day at a time, other databases'
// rows untouched. It refuses (ErrIncomplete) while any source row is not
// copied, and never touches the destination's history.
func Cleanup(ctx context.Context, src, dst Store) (CleanupReport, error) {
	m, err := startMigration(ctx, src, dst, MigrateOptions{})
	if err != nil {
		return CleanupReport{}, err
	}
	defer m.release(ctx)
	for _, table := range historyTables {
		mk, err := readMark(ctx, m.conn, m.id, m.dir, table)
		if err != nil {
			return CleanupReport{}, err
		}
		after, err := rowsAfter(ctx, m.src, table, mk)
		if err != nil {
			return CleanupReport{}, err
		}
		if after || (mk.found && !mk.completed) {
			return CleanupReport{}, fmt.Errorf("%w: sage.%s", ErrIncomplete, table)
		}
	}
	rep := CleanupReport{Removed: map[string]int64{}}
	for _, table := range historyTables {
		n, err := m.removeSource(ctx, table)
		if err != nil {
			return rep, err
		}
		rep.Removed[table] = n
	}
	if _, err := m.conn.Exec(ctx, `DELETE FROM sage.history_migration_ids
		WHERE database_id = $1 AND direction = $2`, m.id, string(m.dir)); err != nil {
		return rep, fmt.Errorf("histstore: drop the snapshot id map: %w", err)
	}
	return rep, nil
}

// removeSource removes one table's source rows.
func (m *migration) removeSource(ctx context.Context, table string) (int64, error) {
	var n int64
	err := m.src.QueryRow(ctx, `SELECT count(*) FROM sage.`+table+` h WHERE {db:h}`).Scan(&n)
	if err != nil || n == 0 {
		return 0, wrapRemove(table, err)
	}
	if !m.src.Scoped() && !m.src.legacyNull {
		// The monitored database's own tables hold nothing else.
		_, err = m.src.Exec(ctx, "TRUNCATE sage."+table)
		return n, wrapRemove(table, err)
	}
	col := "collected_at"
	if table == "query_store" {
		col = "captured_at"
	}
	var lo, hi *time.Time
	if err := m.src.QueryRow(ctx, `SELECT min(h.`+col+`), max(h.`+col+`) FROM sage.`+
		table+` h WHERE {db:h}`).Scan(&lo, &hi); err != nil || lo == nil {
		return n, wrapRemove(table, err)
	}
	for from := lo.Truncate(cleanupDayBatch); !from.After(*hi); from = from.Add(
		cleanupDayBatch) {
		if _, err := m.src.Exec(ctx, `DELETE FROM sage.`+table+` h WHERE {db:h} AND h.`+col+
			` >= $1 AND h.`+col+` < $2`, from, from.Add(cleanupDayBatch)); err != nil {
			return n, wrapRemove(table, err)
		}
	}
	return n, nil
}

func wrapRemove(table string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("histstore: remove the migrated rows of sage.%s: %w", table, err)
}
