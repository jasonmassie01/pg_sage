package histstore

import "context"

// TableStatus is where one table's migration stands.
type TableStatus struct {
	Table   string
	Copied  int64
	Skipped int64
	// Complete: a run reached the end and no source row was written since
	// (or there was never anything to copy).
	Complete bool
	// Pending: the source holds rows the destination does not have.
	Pending bool
}

// Status reads the progress of the migration from src to dst without
// changing anything.
func Status(ctx context.Context, src, dst Store) ([]TableStatus, error) {
	dir, id, err := placement(src, dst)
	if err != nil {
		return nil, err
	}
	if src, err = withLegacy(ctx, src); err != nil {
		return nil, err
	}
	out := make([]TableStatus, 0, len(historyTables))
	for _, table := range historyTables {
		m, err := readMark(ctx, dst.db, id, dir, table)
		if err != nil {
			return nil, err
		}
		pending, err := rowsAfter(ctx, src, table, m)
		if err != nil {
			return nil, err
		}
		// Nothing to copy (no progress, no source row) counts as complete.
		ts := TableStatus{Table: table, Pending: pending,
			Complete: !pending && (m.completed || !m.found)}
		if m.found {
			ts.Copied, ts.Skipped, err = readCounts(ctx, dst.db, id, dir, table)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, ts)
	}
	return out, nil
}

func readCounts(ctx context.Context, db any, id int, dir Mode, table string) (int64, int64,
	error) {
	q, ok := db.(rowQuerier)
	if !ok {
		return 0, 0, ErrNoDatabase
	}
	var copied, skipped int64
	err := q.QueryRow(ctx, `SELECT copied, skipped FROM sage.history_migration
		WHERE database_id = $1 AND direction = $2 AND table_name = $3`,
		id, string(dir), table).Scan(&copied, &skipped)
	return copied, skipped, err
}
