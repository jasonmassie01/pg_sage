package histstore

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// The per-query rollup moves with a database's history (evid-01b). Its
// daily tier and hourly coverage are copied after the history tables, in
// (day, queryid) and hour order: the tables have no id, so a table's
// progress is the last copied key (last_at the day or hour, last_id the
// queryid) and a row already in the destination is skipped by its unique
// key. The hourly tier is not copied: it is derived from the snapshots
// (kept at most retention.query_rollup_hourly_days), so the caller
// rebuilds it once the copy is complete (MigrateOptions.Rebuild).

// derivedTables are the rollup tables that move, in copy order.
var derivedTables = []string{"query_rollup_daily", "query_rollup_coverage"}

// RollupTables are the per-query rollup's tables (size accounting).
var RollupTables = []string{"query_rollup_hourly", "query_rollup_daily",
	"query_rollup_coverage"}

// movedTables are every table a migration copies, in copy order.
func movedTables() []string {
	return append(append([]string{}, historyTables...),
		derivedTables...)
}

// derivedSourceSQL streams the rows after the key ($1, $2) in key order,
// each as its key and a JSON document without database_id.
var derivedSourceSQL = map[string]string{
	"query_rollup_daily": `/* pg_sage */ SELECT r.day::timestamp AT TIME ZONE 'UTC',
		r.queryid, (to_jsonb(r) - 'database_id')::text FROM sage.query_rollup_daily r
		WHERE {db:r} AND (r.day, r.queryid) > (($1::timestamptz AT TIME ZONE 'UTC')::date,
		$2::int8) ORDER BY r.day, r.queryid`,
	"query_rollup_coverage": `/* pg_sage */ SELECT r.hour, 0::int8,
		(to_jsonb(r) - 'database_id')::text FROM sage.query_rollup_coverage r
		WHERE {db:r} AND r.hour > $1 AND $2::int8 IS NOT NULL ORDER BY r.hour`,
}

// derivedInsertSQL writes a batch ($1, a JSON array) keeping every column
// but database_id (the placement); a row whose key exists is skipped.
var derivedInsertSQL = map[string]string{
	"query_rollup_daily": `/* pg_sage */ INSERT INTO sage.query_rollup_daily (queryid,
		day, calls, exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read,
		temp_blks_written, wal_bytes, hours, epoch_breaks, plan_hashes, last_plan_hash,
		built_at{dbcol})
		SELECT r.queryid, r.day, r.calls, r.exec_ms, r.rows, r.shared_blks_hit,
			r.shared_blks_read, r.temp_blks_read, r.temp_blks_written, r.wal_bytes, r.hours,
			r.epoch_breaks, r.plan_hashes, r.last_plan_hash, r.built_at{dbval}
		FROM jsonb_populate_recordset(NULL::sage.query_rollup_daily, $1::jsonb) r
		ON CONFLICT DO NOTHING`,
	"query_rollup_coverage": `/* pg_sage */ INSERT INTO sage.query_rollup_coverage (hour,
		db_exec_ms, sampled_exec_ms, sampled_queryids, epoch_breaks, dealloc_delta,
		snapshots, epoch_source, built_at{dbcol})
		SELECT r.hour, r.db_exec_ms, r.sampled_exec_ms, r.sampled_queryids, r.epoch_breaks,
			r.dealloc_delta, r.snapshots, r.epoch_source, r.built_at{dbval}
		FROM jsonb_populate_recordset(NULL::sage.query_rollup_coverage, $1::jsonb) r
		ON CONFLICT DO NOTHING`,
}

// derivedRow is one rollup row read from the source.
type derivedRow struct {
	at  time.Time
	qid int64
	doc string
}

// key is the mark's resume key: before every row without a mark.
func (m mark) key() (time.Time, int64) {
	if !m.found || m.lastAt == nil {
		return time.Time{}, math.MinInt64
	}
	return *m.lastAt, m.lastID
}

// copyDerived copies one rollup table's rows after its mark.
func (m *migration) copyDerived(ctx context.Context, table string) (TableReport, error) {
	tr := TableReport{Table: table}
	mk, err := readMark(ctx, m.conn, m.id, m.dir, table)
	if err != nil {
		return tr, err
	}
	at, qid := mk.key()
	rows, err := m.src.Query(ctx, derivedSourceSQL[table], at, qid)
	if err != nil {
		return tr, fmt.Errorf("histstore: read sage.%s: %w", table, err)
	}
	defer rows.Close()
	batch := make([]derivedRow, 0, m.opt.BatchRows)
	for rows.Next() {
		var r derivedRow
		if err := rows.Scan(&r.at, &r.qid, &r.doc); err != nil {
			return tr, fmt.Errorf("histstore: scan sage.%s: %w", table, err)
		}
		if batch = append(batch, r); len(batch) < m.opt.BatchRows {
			continue
		}
		if err := m.flushDerived(ctx, table, batch, &tr); err != nil {
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
	if err := m.flushDerived(ctx, table, batch, &tr); err != nil {
		return tr, err
	}
	tr.Complete = true
	return tr, m.complete(ctx, table)
}

// flushDerived writes one batch and its progress in one destination
// transaction.
func (m *migration) flushDerived(ctx context.Context, table string, batch []derivedRow,
	tr *TableReport) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := m.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("histstore: begin a batch of sage.%s: %w", table, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	docs := make([]string, len(batch))
	for i, r := range batch {
		docs[i] = r.doc
	}
	tag, err := m.dst.WithDB(tx).Exec(ctx, derivedInsertSQL[table],
		"["+strings.Join(docs, ",")+"]")
	if err != nil {
		return fmt.Errorf("histstore: write a batch of sage.%s: %w", table, err)
	}
	last := batch[len(batch)-1]
	if err := m.advance(ctx, tx, table, []sourceRow{{id: last.qid, at: last.at}},
		tag.RowsAffected(), 0); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("histstore: commit a batch of sage.%s: %w", table, err)
	}
	m.batches++
	tr.Copied += tag.RowsAffected()
	m.logf("sage.%s: copied %d rows up to %s", table, tr.Copied,
		last.at.UTC().Format(time.RFC3339))
	return nil
}

// derivedRowsAfter reports whether src holds a rollup row after the mark.
func derivedRowsAfter(ctx context.Context, src Store, table string, m mark) (bool, error) {
	at, qid := m.key()
	sql := `/* pg_sage */ SELECT EXISTS (SELECT 1 FROM sage.query_rollup_coverage r
		WHERE {db:r} AND r.hour > $1 AND $2::int8 IS NOT NULL)`
	if table == "query_rollup_daily" {
		sql = `/* pg_sage */ SELECT EXISTS (SELECT 1 FROM sage.query_rollup_daily r
			WHERE {db:r} AND (r.day, r.queryid) >
			(($1::timestamptz AT TIME ZONE 'UTC')::date, $2::int8))`
	}
	var after bool
	if err := src.QueryRow(ctx, sql, at, qid).Scan(&after); err != nil {
		return false, fmt.Errorf("look for unmigrated rows of sage.%s: %w", table, err)
	}
	return after, nil
}

// rebuild runs the caller's rebuild of the derived hourly tier once every
// table is copied.
func (m *migration) rebuild(ctx context.Context, rep *MigrateReport) error {
	if m.opt.Rebuild == nil {
		return nil
	}
	for _, tr := range rep.Tables {
		if !tr.Complete {
			return nil
		}
	}
	if err := m.opt.Rebuild(ctx, m.out); err != nil {
		return fmt.Errorf("histstore: rebuild the hourly query rollup in the "+
			"destination: %w", err)
	}
	rep.HourlyRebuilt = true
	m.logf("sage.query_rollup_hourly: hourly rollup rebuilt from the copied snapshots")
	return nil
}
