package histstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/partition"
)

// sourceRow is one history row read from the source.
type sourceRow struct {
	id     int64
	at     time.Time
	cat    string  // snapshots
	data   string  // snapshots: the document as JSON text
	baseID *int64  // snapshots
	qs     qsValue // query_store
}

type qsValue struct {
	queryid, calls, rows int64
	total, mean          float64
	planHash             *string
	epoch                *time.Time
}

// sourceSQL streams the rows past the mark in id order: a merge of the
// partitions' primary keys for snapshots, one sort for query_store.
var sourceSQL = map[string]string{
	"snapshots": `/* pg_sage */ SELECT s.id, s.collected_at, s.category, s.data::text,
		s.base_id FROM sage.snapshots s WHERE {db:s} AND s.id > $1 ORDER BY s.id`,
	"query_store": `/* pg_sage */ SELECT q.id, q.captured_at, q.queryid, q.calls,
		q.total_exec_time, q.mean_exec_time, q.rows, q.plan_hash, q.stats_epoch
		FROM sage.query_store q WHERE {db:q} AND q.id > $1 ORDER BY q.id`,
}

// spanSQL is the source's time span past the mark (by index), for the
// destination's day partitions.
var spanSQL = map[string]string{
	"snapshots": `/* pg_sage */ SELECT min(s.collected_at), max(s.collected_at)
		FROM sage.snapshots s WHERE {db:s} AND s.id > $1`,
	"query_store": `/* pg_sage */ SELECT min(q.captured_at), max(q.captured_at)
		FROM sage.query_store q WHERE {db:q} AND q.id > $1`,
}

var tablesByName = map[string]partition.Table{
	"snapshots": partition.Snapshots, "query_store": partition.QueryStore,
}

// copyTable copies one table's rows past its mark.
func (m *migration) copyTable(ctx context.Context, table string) (TableReport, error) {
	tr := TableReport{Table: table}
	mk, err := readMark(ctx, m.conn, m.id, m.dir, table)
	if err != nil {
		return tr, err
	}
	if err := m.ensureDays(ctx, table, mk.lastID); err != nil {
		return tr, err
	}
	rows, err := m.src.Query(ctx, sourceSQL[table], mk.lastID)
	if err != nil {
		return tr, fmt.Errorf("histstore: read sage.%s: %w", table, err)
	}
	defer rows.Close()
	batch := make([]sourceRow, 0, m.opt.BatchRows)
	for rows.Next() {
		r, err := scanSource(rows, table)
		if err != nil {
			return tr, err
		}
		if batch = append(batch, r); len(batch) < m.opt.BatchRows {
			continue
		}
		if err := m.flush(ctx, table, batch, &tr); err != nil {
			return tr, err
		}
		batch = batch[:0]
		if m.opt.MaxBatches > 0 && m.batches >= m.opt.MaxBatches {
			return tr, nil // stopped early: resumed by the next run
		}
	}
	if err := rows.Err(); err != nil {
		return tr, fmt.Errorf("histstore: read sage.%s: %w", table, err)
	}
	if err := m.flush(ctx, table, batch, &tr); err != nil {
		return tr, err
	}
	tr.Complete = true
	return tr, m.complete(ctx, table)
}

func scanSource(rows pgx.Rows, table string) (sourceRow, error) {
	var r sourceRow
	var err error
	if table == "snapshots" {
		err = rows.Scan(&r.id, &r.at, &r.cat, &r.data, &r.baseID)
	} else {
		q := &r.qs
		err = rows.Scan(&r.id, &r.at, &q.queryid, &q.calls, &q.total, &q.mean, &q.rows,
			&q.planHash, &q.epoch)
	}
	if err != nil {
		return r, fmt.Errorf("histstore: scan sage.%s: %w", table, err)
	}
	return r, nil
}

// ensureDays creates the destination's day partitions for the span to be
// copied, so old rows land in their days and not in the default
// partition (a plain destination table is left alone).
func (m *migration) ensureDays(ctx context.Context, table string, lastID int64) error {
	var lo, hi *time.Time
	if err := m.src.QueryRow(ctx, spanSQL[table], lastID).Scan(&lo, &hi); err != nil {
		return fmt.Errorf("histstore: read the span of sage.%s: %w", table, err)
	}
	if lo == nil || hi == nil {
		return nil
	}
	days := int(partition.DayStart(*hi).Sub(partition.DayStart(*lo))/(24*time.Hour)) + 2
	if _, err := partition.Ensure(ctx, m.conn, tablesByName[table], *lo, days); err != nil {
		return fmt.Errorf("histstore: day partitions of the destination: %w", err)
	}
	return nil
}

// flush writes one batch and its progress in one destination transaction.
func (m *migration) flush(ctx context.Context, table string, batch []sourceRow,
	tr *TableReport) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := m.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("histstore: begin a batch: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	dst := m.dst.WithDB(tx)
	var copied, skipped int64
	if table == "snapshots" {
		copied, skipped, err = m.writeSnapshots(ctx, tx, dst, batch)
	} else {
		copied, err = writeSamples(ctx, dst, batch)
	}
	if err != nil {
		return err
	}
	if err := m.advance(ctx, tx, table, batch, copied, skipped); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("histstore: commit a batch of sage.%s: %w", table, err)
	}
	m.batches++
	tr.Copied += copied
	tr.Skipped += skipped
	m.logf("sage.%s: copied %d rows up to id %d", table, tr.Copied, batch[len(batch)-1].id)
	return nil
}

// advance records the batch in the progress row; completed_at is cleared
// until the run reaches the end.
func (m *migration) advance(ctx context.Context, tx pgx.Tx, table string, batch []sourceRow,
	copied, skipped int64) error {
	last := batch[len(batch)-1]
	newest := last.at
	for _, r := range batch {
		if r.at.After(newest) {
			newest = r.at
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO sage.history_migration AS h (database_id, direction,
		table_name, last_id, last_at, copied, skipped) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (database_id, direction, table_name) DO UPDATE SET
		  last_id = EXCLUDED.last_id,
		  last_at = GREATEST(h.last_at, EXCLUDED.last_at),
		  copied = h.copied + EXCLUDED.copied, skipped = h.skipped + EXCLUDED.skipped,
		  updated_at = now(), completed_at = NULL`,
		m.id, string(m.dir), table, last.id, newest, copied, skipped)
	if err != nil {
		return fmt.Errorf("histstore: record the progress of sage.%s: %w", table, err)
	}
	return nil
}

// complete marks the table copied to the end (creating the progress row of
// an empty source).
func (m *migration) complete(ctx context.Context, table string) error {
	_, err := m.conn.Exec(ctx, `INSERT INTO sage.history_migration (database_id, direction,
		table_name, completed_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (database_id, direction, table_name) DO UPDATE SET
		  completed_at = now(), updated_at = now()`, m.id, string(m.dir), table)
	if err != nil {
		return fmt.Errorf("histstore: record sage.%s as copied: %w", table, err)
	}
	return nil
}

// writeSamples inserts a batch of query_store samples.
func writeSamples(ctx context.Context, dst Store, batch []sourceRow) (int64, error) {
	n := len(batch)
	at, epochs := make([]time.Time, n), make([]*time.Time, n)
	qids, calls, rowsN := make([]int64, n), make([]int64, n), make([]int64, n)
	totals, means, hashes := make([]float64, n), make([]float64, n), make([]*string, n)
	for i, r := range batch {
		at[i], qids[i], calls[i], rowsN[i] = r.at, r.qs.queryid, r.qs.calls, r.qs.rows
		totals[i], means[i], hashes[i], epochs[i] = r.qs.total, r.qs.mean, r.qs.planHash,
			r.qs.epoch
	}
	tag, err := dst.Exec(ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
		total_exec_time, mean_exec_time, rows, plan_hash, stats_epoch{dbcol})
		SELECT u.at, u.qid, u.calls, u.total, u.mean, u.rows, u.hash, u.epoch{dbval}
		FROM unnest($1::timestamptz[], $2::int8[], $3::int8[], $4::float8[], $5::float8[],
		            $6::int8[], $7::text[], $8::timestamptz[])
		     AS u(at, qid, calls, total, mean, rows, hash, epoch)`,
		at, qids, calls, totals, means, rowsN, hashes, epochs)
	if err != nil {
		return 0, fmt.Errorf("histstore: write query_store samples: %w", err)
	}
	return tag.RowsAffected(), nil
}
