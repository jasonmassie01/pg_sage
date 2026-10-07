package histstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// writeSnapshots inserts a batch of snapshots with destination ids taken
// up front, so a delta's base (an earlier row of this batch or of an
// earlier batch) maps to its new id. A delta whose base was never copied
// (gone from the source: it read as NULL there too) is skipped and
// counted.
func (m *migration) writeSnapshots(ctx context.Context, tx pgx.Tx, dst Store,
	batch []sourceRow) (copied, skipped int64, err error) {
	mapped, err := m.mappedBases(ctx, tx, batch)
	if err != nil {
		return 0, 0, err
	}
	ids, err := newSnapshotIDs(ctx, tx, len(batch))
	if err != nil {
		return 0, 0, err
	}
	var w snapshotWrite
	for i, r := range batch {
		var base *int64
		if r.baseID != nil {
			b, ok := mapped[*r.baseID]
			if !ok {
				skipped++
				continue
			}
			base = &b
		}
		mapped[r.id] = ids[i]
		w.add(r, ids[i], base)
	}
	if len(w.src) == 0 {
		return 0, skipped, nil
	}
	if err := w.insert(ctx, dst); err != nil {
		return 0, 0, err
	}
	if err := m.recordIDs(ctx, tx, w.src, w.ids); err != nil {
		return 0, 0, err
	}
	return int64(len(w.src)), skipped, nil
}

// snapshotWrite is a batch's rows as insert arrays.
type snapshotWrite struct {
	src, ids, bases []int64
	nullBase        []bool
	at              []time.Time
	cats, docs      []string
}

func (w *snapshotWrite) add(r sourceRow, id int64, base *int64) {
	w.src, w.ids, w.at = append(w.src, r.id), append(w.ids, id), append(w.at, r.at)
	w.cats, w.docs = append(w.cats, r.cat), append(w.docs, r.data)
	if base == nil {
		w.bases, w.nullBase = append(w.bases, 0), append(w.nullBase, true)
		return
	}
	w.bases, w.nullBase = append(w.bases, *base), append(w.nullBase, false)
}

func (w *snapshotWrite) insert(ctx context.Context, dst Store) error {
	_, err := dst.Exec(ctx, `INSERT INTO sage.snapshots (id, collected_at, category, data,
		base_id{dbcol})
		SELECT u.id, u.at, u.cat, u.doc::jsonb, CASE WHEN u.nobase THEN NULL ELSE u.base END
		       {dbval}
		FROM unnest($1::int8[], $2::timestamptz[], $3::text[], $4::text[], $5::int8[],
		            $6::bool[]) AS u(id, at, cat, doc, base, nobase)`,
		w.ids, w.at, w.cats, w.docs, w.bases, w.nullBase)
	if err != nil {
		return fmt.Errorf("histstore: write snapshots: %w", err)
	}
	return nil
}

// mappedBases maps the batch's bases that earlier batches copied to their
// destination ids.
func (m *migration) mappedBases(ctx context.Context, tx pgx.Tx, batch []sourceRow) (
	map[int64]int64, error) {
	var want []int64
	for _, r := range batch {
		if r.baseID != nil {
			want = append(want, *r.baseID)
		}
	}
	out := make(map[int64]int64, len(batch))
	if len(want) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT source_id, dest_id FROM sage.history_migration_ids
		WHERE database_id = $1 AND direction = $2 AND source_id = ANY($3::int8[])`,
		m.id, string(m.dir), want)
	if err != nil {
		return nil, fmt.Errorf("histstore: read the snapshot id map: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var src, dst int64
		if err := rows.Scan(&src, &dst); err != nil {
			return nil, fmt.Errorf("histstore: read the snapshot id map: %w", err)
		}
		out[src] = dst
	}
	return out, rows.Err()
}

// newSnapshotIDs takes n ids from the destination's snapshot sequence.
func newSnapshotIDs(ctx context.Context, tx pgx.Tx, n int) ([]int64, error) {
	rows, err := tx.Query(ctx, `SELECT nextval(pg_get_serial_sequence('sage.snapshots', 'id'))
		FROM generate_series(1, $1)`, n)
	if err != nil {
		return nil, fmt.Errorf("histstore: take snapshot ids: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("histstore: take snapshot ids: %w", err)
	}
	return ids, nil
}

// recordIDs keeps the batch's id map for deltas of later batches and runs.
func (m *migration) recordIDs(ctx context.Context, tx pgx.Tx, src, dst []int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO sage.history_migration_ids (database_id, direction,
		source_id, dest_id) SELECT $1, $2, s, d FROM unnest($3::int8[], $4::int8[]) AS u(s, d)
		ON CONFLICT (database_id, direction, source_id) DO UPDATE SET dest_id = EXCLUDED.dest_id`,
		m.id, string(m.dir), src, dst)
	if err != nil {
		return fmt.Errorf("histstore: record the snapshot id map: %w", err)
	}
	return nil
}
