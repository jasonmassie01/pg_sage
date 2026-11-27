package analyzer

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// unusedClockTimeout bounds each clock statement (the history decode of
// at most window+1 documents is the largest).
const unusedClockTimeout = 30 * time.Second

// pgUnusedClockDB persists the clock in the monitored database's sage
// schema; history is read from the pool's history store.
type pgUnusedClockDB struct {
	pool *pgxpool.Pool
}

// newPGUnusedClockDB returns nil without a pool: the clock is then kept
// in memory only, as before rules-04.
func newPGUnusedClockDB(pool *pgxpool.Pool) unusedClockDB {
	if pool == nil {
		return nil
	}
	return &pgUnusedClockDB{pool: pool}
}

const unusedClockLoadSQL = `/* pg_sage analyzer */
SELECT ident, indexrelid::int8, zero_since FROM sage.unused_index_clock`

// Legacy rows without usage_evidence are zero-scan findings.
const unusedOpenZeroScanSQL = `/* pg_sage analyzer */
SELECT DISTINCT object_identifier FROM sage.findings
 WHERE category = 'unused_index' AND status = 'open'
   AND object_identifier IS NOT NULL
   AND COALESCE(detail->>'usage_evidence', 'zero_scans') = 'zero_scans'`

// unusedClockSaveSQL deletes $4 and upserts ($1, $2, $3) in one statement.
const unusedClockSaveSQL = `/* pg_sage analyzer */
WITH gone AS (
    DELETE FROM sage.unused_index_clock WHERE ident = ANY($4::text[])
)
INSERT INTO sage.unused_index_clock (ident, indexrelid, zero_since, updated_at)
SELECT u.ident, u.oid::oid, u.since, now()
  FROM unnest($1::text[], $2::int8[], $3::timestamptz[]) AS u(ident, oid, since)
ON CONFLICT (ident) DO UPDATE
   SET indexrelid = EXCLUDED.indexrelid, zero_since = EXCLUDED.zero_since,
       updated_at = EXCLUDED.updated_at`

// unusedHistoryPicksSQL picks the first non-empty 'indexes' snapshot of
// each day starting at $1, newest first.
var unusedHistoryPicksSQL = `/* pg_sage analyzer */
SELECT p.id, p.collected_at
  FROM unnest($1::timestamptz[]) AS b(lo)
 CROSS JOIN LATERAL (
       SELECT s.id, s.collected_at FROM sage.snapshots s
        WHERE {db:s} AND s.category = 'indexes'
          AND s.collected_at >= b.lo AND s.collected_at < b.lo + interval '1 day'
          AND ` + snapstore.NonEmptySQL("s") + `
        ORDER BY s.collected_at, s.id LIMIT 1) p
 ORDER BY p.collected_at DESC`

// unusedHistoryDecodeSQL reads idents $2 from the snapshots $1. Elements
// without an integer oid and scan count are skipped (no evidence).
var unusedHistoryDecodeSQL = `/* pg_sage analyzer */
WITH d AS MATERIALIZED (
    SELECT s.id, ` + snapstore.DataSQL("s") + ` AS doc
      FROM sage.snapshots s WHERE {db:s} AND s.id = ANY($1::int8[])
)
SELECT d.id, (e->>'schemaname') || '.' || (e->>'indexrelname'),
       (e->>'indexrelid')::int8, (e->>'idx_scan')::int8
  FROM d
 CROSS JOIN LATERAL jsonb_array_elements(
           CASE WHEN jsonb_typeof(d.doc) = 'array' THEN d.doc
                ELSE '[]'::jsonb END) AS e
 WHERE (e->>'schemaname') || '.' || (e->>'indexrelname') = ANY($2::text[])
   AND (e->>'indexrelid') ~ '^[0-9]+$' AND (e->>'idx_scan') ~ '^[0-9]+$'`

func (p *pgUnusedClockDB) load(ctx context.Context) (map[string]clockRow, error) {
	ctx, cancel := context.WithTimeout(ctx, unusedClockTimeout)
	defer cancel()
	rows, err := p.pool.Query(ctx, unusedClockLoadSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]clockRow{}
	for rows.Next() {
		var ident string
		var oid int64
		var since time.Time
		if err := rows.Scan(&ident, &oid, &since); err != nil {
			return nil, fmt.Errorf("scan clock row: %w", err)
		}
		out[ident] = clockRow{oid: uint32(oid), since: since}
	}
	return out, rows.Err()
}

func (p *pgUnusedClockDB) openZeroScan(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, unusedClockTimeout)
	defer cancel()
	rows, err := p.pool.Query(ctx, unusedOpenZeroScanSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ident string
		if err := rows.Scan(&ident); err != nil {
			return nil, fmt.Errorf("scan open finding: %w", err)
		}
		out = append(out, ident)
	}
	return out, rows.Err()
}

func (p *pgUnusedClockDB) save(ctx context.Context, up map[string]clockRow,
	del []string) error {
	ctx, cancel := context.WithTimeout(ctx, unusedClockTimeout)
	defer cancel()
	idents := make([]string, 0, len(up))
	oids := make([]int64, 0, len(up))
	since := make([]time.Time, 0, len(up))
	for ident, row := range up {
		idents = append(idents, ident)
		oids = append(oids, int64(row.oid))
		since = append(since, row.since)
	}
	if del == nil {
		del = []string{}
	}
	_, err := p.pool.Exec(ctx, unusedClockSaveSQL, idents, oids, since, del)
	return err
}

func (p *pgUnusedClockDB) history(ctx context.Context, idents []string, now time.Time,
	days int) ([]indexHistoryDoc, error) {
	ctx, cancel := context.WithTimeout(ctx, unusedClockTimeout)
	defer cancel()
	los := make([]time.Time, 0, days)
	for k := 1; k <= days; k++ {
		los = append(los, now.Add(-time.Duration(k)*24*time.Hour))
	}
	store := histstore.Resolve(p.pool)
	rows, err := store.Query(ctx, unusedHistoryPicksSQL, los)
	if err != nil {
		return nil, fmt.Errorf("pick daily index snapshots: %w", err)
	}
	var docs []indexHistoryDoc
	var ids []int64
	at := map[int64]int{}
	for rows.Next() {
		var id int64
		var d indexHistoryDoc
		if err := rows.Scan(&id, &d.at); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan index snapshot: %w", err)
		}
		d.indexes = map[string]historyIndex{}
		at[id] = len(docs)
		docs, ids = append(docs, d), append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pick daily index snapshots: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return docs, decodeIndexHistory(ctx, store, ids, idents, docs, at)
}

// decodeIndexHistory fills docs (by snapshot id via at) with idents' oid
// and scan count.
func decodeIndexHistory(ctx context.Context, store histstore.Store, ids []int64,
	idents []string, docs []indexHistoryDoc, at map[int64]int) error {
	rows, err := store.Query(ctx, unusedHistoryDecodeSQL, ids, idents)
	if err != nil {
		return fmt.Errorf("decode index snapshots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, oid, scans int64
		var ident string
		if err := rows.Scan(&id, &ident, &oid, &scans); err != nil {
			return fmt.Errorf("scan index history: %w", err)
		}
		docs[at[id]].indexes[ident] = historyIndex{oid: uint32(oid), scans: scans}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("decode index snapshots: %w", err)
	}
	return nil
}
