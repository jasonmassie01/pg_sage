package workpack

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/snapstore"
)

// The history reads of the evid-02b sections, in the build's read-only
// history transaction: the window's first and last indexes and tables
// snapshots with the relation-statistics epoch between them, the daily
// config and tables picks, and the system snapshots the settings'
// measures come from.

// The states of the window's relation counters.
const (
	relStatusOK      = "ok"
	relStatusReset   = "reset"
	relStatusUnknown = "unknown"
)

// tableCounters is one table's counters in one tables snapshot.
type tableCounters struct {
	ins, upd, del, av, aa int64
	lastAV, lastAA        *time.Time
	reloptions            []string
}

// relWindow is the indexes and tables counters at the window's first and
// last snapshot of each, keyed by "schema.name", and whether they are
// comparable: ok (one relation-statistics epoch throughout), reset or
// unknown (fewer than two snapshots, or an unrecorded epoch).
type relWindow struct {
	status            string
	idxFirst, idxLast map[string]int64
	tabFirst, tabLast map[string]tableCounters
}

// maintHistory is what the evid-02b sections read from the history.
type maintHistory struct {
	rel      relWindow
	settings []settingsPick
	measures Measures
	dead     map[string]map[string]int64
	conn     map[int]*connHourAgg
}

// edge is the window's first or last snapshot of a category.
type edge struct {
	id int64
	at time.Time
}

var edgesSQL = `/* pg_sage */ (SELECT s.id, s.collected_at FROM sage.snapshots s
  WHERE {db:s} AND s.category = $1 AND s.collected_at >= $2 AND s.collected_at < $3
    AND ` + snapstore.NonEmptySQL("s") + ` ORDER BY s.collected_at, s.id LIMIT 1)
UNION ALL
(SELECT s.id, s.collected_at FROM sage.snapshots s
  WHERE {db:s} AND s.category = $1 AND s.collected_at >= $2 AND s.collected_at < $3
    AND ` + snapstore.NonEmptySQL("s") + ` ORDER BY s.collected_at DESC, s.id DESC LIMIT 1)`

// edges returns the window's first and last snapshot of category; ok is
// false unless they are two different snapshots.
func (r reader) edges(ctx context.Context, category string) (first, last edge, ok bool,
	err error) {
	rows, err := r.st.Query(ctx, edgesSQL, category, r.w.From, r.w.To)
	if err != nil {
		return edge{}, edge{}, false, r.fail("sage.snapshots ("+category+")", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (edge, error) {
		var e edge
		err := row.Scan(&e.id, &e.at)
		return e, err
	})
	if err != nil {
		return edge{}, edge{}, false, r.fail("sage.snapshots ("+category+")", err)
	}
	if len(out) != 2 || out[0].id == out[1].id {
		return edge{}, edge{}, false, nil
	}
	return out[0], out[1], true, nil
}

var indexCountersSQL = `/* pg_sage */ SELECT e->>'schemaname', e->>'indexrelname',
       (e->>'idx_scan')::int8
  FROM sage.snapshots s
 CROSS JOIN LATERAL jsonb_array_elements(` + snapstore.DataSQL("s") + `) e
 WHERE {db:s} AND s.id = $1 AND e->>'idx_scan' IS NOT NULL`

func (r reader) indexCounters(ctx context.Context, id int64) (map[string]int64, error) {
	rows, err := r.st.Query(ctx, indexCountersSQL, id)
	if err != nil {
		return nil, r.fail("sage.snapshots (indexes)", err)
	}
	out := map[string]int64{}
	for rows.Next() {
		var schema, name string
		var scans int64
		if err := rows.Scan(&schema, &name, &scans); err != nil {
			rows.Close()
			return nil, r.fail("sage.snapshots (indexes)", err)
		}
		out[schema+"."+name] = scans
	}
	if err := rows.Err(); err != nil {
		return nil, r.fail("sage.snapshots (indexes)", err)
	}
	return out, nil
}

var tableCountersSQL = `/* pg_sage */ SELECT e->>'schemaname', e->>'relname',
       COALESCE((e->>'n_tup_ins')::int8, 0), COALESCE((e->>'n_tup_upd')::int8, 0),
       COALESCE((e->>'n_tup_del')::int8, 0), COALESCE((e->>'autovacuum_count')::int8, 0),
       COALESCE((e->>'autoanalyze_count')::int8, 0),
       (e->>'last_autovacuum')::timestamptz, (e->>'last_autoanalyze')::timestamptz,
       ARRAY(SELECT jsonb_array_elements_text(CASE WHEN jsonb_typeof(e->'reloptions') = 'array'
             THEN e->'reloptions' ELSE '[]'::jsonb END))
  FROM sage.snapshots s
 CROSS JOIN LATERAL jsonb_array_elements(` + snapstore.DataSQL("s") + `) e
 WHERE {db:s} AND s.id = $1`

func (r reader) tableCounters(ctx context.Context, id int64) (map[string]tableCounters,
	error) {
	rows, err := r.st.Query(ctx, tableCountersSQL, id)
	if err != nil {
		return nil, r.fail("sage.snapshots (tables)", err)
	}
	out := map[string]tableCounters{}
	for rows.Next() {
		var schema, name string
		var c tableCounters
		if err := rows.Scan(&schema, &name, &c.ins, &c.upd, &c.del, &c.av, &c.aa, &c.lastAV,
			&c.lastAA, &c.reloptions); err != nil {
			rows.Close()
			return nil, r.fail("sage.snapshots (tables)", err)
		}
		out[schema+"."+name] = c
	}
	if err := rows.Err(); err != nil {
		return nil, r.fail("sage.snapshots (tables)", err)
	}
	return out, nil
}

// epochSQL counts the system snapshots in [$1, $2], their distinct
// relation-statistics epochs and those without one.
var epochSQL = `/* pg_sage */ SELECT count(*), count(DISTINCT x.d->>'relation_stats_epoch'),
       count(*) FILTER (WHERE COALESCE(x.d->>'relation_stats_epoch', '') = '')
  FROM (SELECT ` + snapstore.DataSQL("s") + ` AS d FROM sage.snapshots s
         WHERE {db:s} AND s.category = 'system' AND s.collected_at >= $1
           AND s.collected_at <= $2) x`

// epochStatus says whether the relation counters are comparable across
// [from, to].
func (r reader) epochStatus(ctx context.Context, from, to time.Time) (string, error) {
	var n, distinct, missing int64
	if err := r.st.QueryRow(ctx, epochSQL, from, to).Scan(&n, &distinct,
		&missing); err != nil {
		return "", r.fail("sage.snapshots (system)", err)
	}
	switch {
	case n == 0 || missing > 0:
		return relStatusUnknown, nil
	case distinct > 1:
		return relStatusReset, nil
	}
	return relStatusOK, nil
}

// relations reads the relation counters at the window's edges.
func (r reader) relations(ctx context.Context, h *history) error {
	win := relWindow{status: relStatusUnknown, idxFirst: map[string]int64{},
		idxLast: map[string]int64{}, tabFirst: map[string]tableCounters{},
		tabLast: map[string]tableCounters{}}
	iFirst, iLast, iOK, err := r.edges(ctx, "indexes")
	if err != nil {
		return err
	}
	tFirst, tLast, tOK, err := r.edges(ctx, "tables")
	if err != nil {
		return err
	}
	if err := r.relationCounters(ctx, &win, iFirst, iLast, iOK, tFirst, tLast,
		tOK); err != nil {
		return err
	}
	if iOK || tOK {
		from, to := spanOf(iFirst, iLast, iOK, tFirst, tLast, tOK)
		if win.status, err = r.epochStatus(ctx, from, to); err != nil {
			return err
		}
	}
	h.maint.rel = win
	return nil
}

func (r reader) relationCounters(ctx context.Context, win *relWindow, iFirst, iLast edge,
	iOK bool, tFirst, tLast edge, tOK bool) error {
	var err error
	if iOK {
		if win.idxFirst, err = r.indexCounters(ctx, iFirst.id); err != nil {
			return err
		}
		if win.idxLast, err = r.indexCounters(ctx, iLast.id); err != nil {
			return err
		}
	}
	if tOK {
		if win.tabFirst, err = r.tableCounters(ctx, tFirst.id); err != nil {
			return err
		}
		if win.tabLast, err = r.tableCounters(ctx, tLast.id); err != nil {
			return err
		}
	}
	return nil
}

// spanOf is the instants the compared snapshots span.
func spanOf(iFirst, iLast edge, iOK bool, tFirst, tLast edge, tOK bool) (time.Time,
	time.Time) {
	switch {
	case iOK && tOK:
		from, to := iFirst.at, iLast.at
		if tFirst.at.Before(from) {
			from = tFirst.at
		}
		if tLast.at.After(to) {
			to = tLast.at
		}
		return from, to
	case iOK:
		return iFirst.at, iLast.at
	}
	return tFirst.at, tLast.at
}
