package workpack

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/workload"
)

// The snapshot reads of a build: each UTC day's last non-empty snapshot of
// a category in the window (at most 100 per category), decoded through
// the snapshot accessor. Statement texts come from the queries picks (a
// queryid's latest), the family catalog and the largest tables from the
// last tables pick, the size series from every system and tables pick.

// picksSQL returns each UTC day's last non-empty snapshot of category $1
// in [$2, $3), oldest day first.
var picksSQL = `/* pg_sage */ SELECT DISTINCT ON (p.day) p.day, p.id FROM (
       SELECT (s.collected_at AT TIME ZONE 'UTC')::date AS day, s.id, s.collected_at
         FROM sage.snapshots s
        WHERE {db:s} AND s.category = $1 AND s.collected_at >= $2 AND s.collected_at < $3
          AND ` + snapstore.NonEmptySQL("s") + `) p
 ORDER BY p.day, p.collected_at DESC, p.id DESC`

// textsSQL is each queryid of $2's latest text in the snapshots $1.
var textsSQL = `/* pg_sage */ SELECT DISTINCT ON (q.queryid) q.queryid, q.query FROM (
       SELECT (e->>'queryid')::int8 AS queryid, e->>'query' AS query, s.collected_at, s.id
         FROM sage.snapshots s
        CROSS JOIN LATERAL jsonb_array_elements(` + snapstore.DataSQL("s") + `) e
        WHERE {db:s} AND s.id = ANY($1::int8[])) q
 WHERE q.queryid = ANY($2::int8[]) AND q.query IS NOT NULL
 ORDER BY q.queryid, q.collected_at DESC, q.id DESC`

// tablesSQL is every table of the tables snapshot $1 with its size.
var tablesSQL = `/* pg_sage */ SELECT e->>'schemaname', e->>'relname',
       COALESCE((e->>'total_bytes')::int8, 0)
  FROM sage.snapshots s
 CROSS JOIN LATERAL jsonb_array_elements(` + snapstore.DataSQL("s") + `) e
 WHERE {db:s} AND s.id = $1`

// tableSizesSQL is the size of the tables ($2, $3) in each snapshot $1.
var tableSizesSQL = `/* pg_sage */ SELECT s.id, e->>'schemaname', e->>'relname',
       (e->>'total_bytes')::int8
  FROM sage.snapshots s
 CROSS JOIN LATERAL jsonb_array_elements(` + snapstore.DataSQL("s") + `) e
 WHERE {db:s} AND s.id = ANY($1::int8[]) AND e->>'total_bytes' IS NOT NULL
   AND (e->>'schemaname', e->>'relname') IN (SELECT * FROM unnest($2::text[], $3::text[]))`

// systemSizesSQL is the database size in each system snapshot $1.
var systemSizesSQL = `/* pg_sage */ SELECT s.id, (` + snapstore.DataSQL("s") +
	`->>'db_size_bytes')::int8
  FROM sage.snapshots s WHERE {db:s} AND s.id = ANY($1::int8[])`

// pick is one day's snapshot.
type pick struct {
	day string
	id  int64
}

func (r reader) picks(ctx context.Context, category string) ([]pick, error) {
	rows, err := r.st.Query(ctx, picksSQL, category, r.w.From, r.w.To)
	if err != nil {
		return nil, r.fail("sage.snapshots ("+category+")", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pick, error) {
		var p pick
		var day time.Time
		err := row.Scan(&day, &p.id)
		p.day = day.Format(time.DateOnly)
		return p, err
	})
	if err != nil {
		return nil, r.fail("sage.snapshots ("+category+")", err)
	}
	return out, nil
}

func pickIDs(ps []pick) []int64 {
	ids := make([]int64, len(ps))
	for i, p := range ps {
		ids[i] = p.id
	}
	return ids
}

// snapshots reads the texts, the catalog and the size series.
func (r reader) snapshots(ctx context.Context, h *history) error {
	if err := r.texts(ctx, h); err != nil {
		return err
	}
	if err := r.tables(ctx, h); err != nil {
		return err
	}
	return r.systemSizes(ctx, h)
}

func (r reader) texts(ctx context.Context, h *history) error {
	h.texts = map[int64]string{}
	qids := map[int64]bool{}
	for _, x := range h.hourly {
		qids[x.qid] = true
	}
	for _, x := range h.daily {
		qids[x.qid] = true
	}
	if len(qids) == 0 {
		return nil
	}
	picks, err := r.picks(ctx, "queries")
	if err != nil || len(picks) == 0 {
		return err
	}
	want := make([]int64, 0, len(qids))
	for q := range qids {
		want = append(want, q)
	}
	rows, err := r.st.Query(ctx, textsSQL, pickIDs(picks), want)
	if err != nil {
		return r.fail("sage.snapshots (queries)", err)
	}
	defer rows.Close()
	for rows.Next() {
		var qid int64
		var text string
		if err := rows.Scan(&qid, &text); err != nil {
			return r.fail("sage.snapshots (queries)", err)
		}
		h.texts[qid] = text
	}
	if err := rows.Err(); err != nil {
		return r.fail("sage.snapshots (queries)", err)
	}
	return nil
}

// tables reads the last pick's catalog and largest tables, then their
// daily sizes.
func (r reader) tables(ctx context.Context, h *history) error {
	h.known, h.tables = map[string][]string{}, map[string]map[string]int64{}
	picks, err := r.picks(ctx, "tables")
	if err != nil || len(picks) == 0 {
		return err
	}
	rows, err := r.st.Query(ctx, tablesSQL, picks[len(picks)-1].id)
	if err != nil {
		return r.fail("sage.snapshots (tables)", err)
	}
	var last []collector.TableStats
	last, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (collector.TableStats,
		error) {
		var t collector.TableStats
		err := row.Scan(&t.SchemaName, &t.RelName, &t.TotalBytes)
		return t, err
	})
	if err != nil {
		return r.fail("sage.snapshots (tables)", err)
	}
	h.known = workload.KnownTables(&collector.Snapshot{Tables: last})
	return r.tableSizes(ctx, h, picks, largestTables(last))
}

// largestTables are the MaxTables largest, by size then name.
func largestTables(ts []collector.TableStats) []collector.TableStats {
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].TotalBytes != ts[j].TotalBytes {
			return ts[i].TotalBytes > ts[j].TotalBytes
		}
		return tableKey(ts[i]) < tableKey(ts[j])
	})
	return ts[:min(len(ts), MaxTables)]
}

func tableKey(t collector.TableStats) string { return t.SchemaName + "." + t.RelName }

func (r reader) tableSizes(ctx context.Context, h *history, picks []pick,
	top []collector.TableStats) error {
	if len(top) == 0 {
		return nil
	}
	schemas, names := make([]string, len(top)), make([]string, len(top))
	for i, t := range top {
		schemas[i], names[i] = t.SchemaName, t.RelName
		h.largest = append(h.largest, tableKey(t))
		h.tables[tableKey(t)] = map[string]int64{}
	}
	dayOf := map[int64]string{}
	for _, p := range picks {
		dayOf[p.id] = p.day
	}
	rows, err := r.st.Query(ctx, tableSizesSQL, pickIDs(picks), schemas, names)
	if err != nil {
		return r.fail("sage.snapshots (tables)", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, bytes int64
		var schema, name string
		if err := rows.Scan(&id, &schema, &name, &bytes); err != nil {
			return r.fail("sage.snapshots (tables)", err)
		}
		h.tables[schema+"."+name][dayOf[id]] = bytes
	}
	if err := rows.Err(); err != nil {
		return r.fail("sage.snapshots (tables)", err)
	}
	return nil
}

func (r reader) systemSizes(ctx context.Context, h *history) error {
	h.dbSize = map[string]int64{}
	picks, err := r.picks(ctx, "system")
	if err != nil || len(picks) == 0 {
		return err
	}
	dayOf := map[int64]string{}
	for _, p := range picks {
		dayOf[p.id] = p.day
	}
	rows, err := r.st.Query(ctx, systemSizesSQL, pickIDs(picks))
	if err != nil {
		return r.fail("sage.snapshots (system)", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var size *int64
		if err := rows.Scan(&id, &size); err != nil {
			return r.fail("sage.snapshots (system)", err)
		}
		if size != nil {
			h.dbSize[dayOf[id]] = *size
		}
	}
	if err := rows.Err(); err != nil {
		return r.fail("sage.snapshots (system)", err)
	}
	return nil
}
