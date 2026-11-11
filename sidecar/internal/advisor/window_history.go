package advisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// windowHistoryTimeout bounds one window-start read of a sub-advisor.
const windowHistoryTimeout = 10 * time.Second

// windowHistory reads the snapshots at the start of the advisor window
// (advisors-05): the WAL and bloat advisors compare the latest snapshot
// with them instead of the previous one, a collector interval old.
type windowHistory interface {
	// readSystemStart is the earliest system row inside the window.
	readSystemStart(ctx context.Context, window time.Duration) (walStart, bool, error)
	// readTablesStart is the earliest tables row inside the window, only
	// the tables keys names.
	readTablesStart(ctx context.Context, window time.Duration, keys []tableKey) (
		tablesStart, bool, error)
}

// walStart is the system row the window starts at.
type walStart struct {
	at  time.Time
	sys collector.SystemStats
}

type tableKey struct{ schema, name string }

// deadCount is a table's tuple counts in a past snapshot.
type deadCount struct{ live, dead int64 }

// tablesStart is the tables row the window starts at.
type tablesStart struct {
	at     time.Time
	tables map[tableKey]deadCount
}

// advisorWindow is the history and window a sub-advisor reasons over.
type advisorWindow struct {
	src    windowHistory // nil: no history (the window is unknown)
	window time.Duration
}

type advisorWindowKey struct{}

func withAdvisorWindow(ctx context.Context, w advisorWindow) context.Context {
	return context.WithValue(ctx, advisorWindowKey{}, w)
}

// advisorWindowFrom is the review's window; without one there is no
// history and the window is fallback.
func advisorWindowFrom(ctx context.Context, fallback time.Duration) advisorWindow {
	w, _ := ctx.Value(advisorWindowKey{}).(advisorWindow)
	if w.window <= 0 {
		w.window = fallback
	}
	return w
}

// windowContext gives the WAL and bloat sub-advisors the history.
func (a *Advisor) windowContext(ctx context.Context) context.Context {
	return withAdvisorWindow(ctx, advisorWindow{src: a.hist, window: a.cfg.Advisor.Interval()})
}

// systemStartSQL is the earliest system row of the window, decoded as
// written (system rows are stored in full).
var systemStartSQL = `/* pg_sage */
SELECT s.collected_at, ` + snapstore.DataSQL("s") + `
  FROM sage.snapshots s
 WHERE {db:s} AND s.category = 'system'
   AND s.collected_at >= now() - make_interval(secs => $1)
 ORDER BY s.collected_at, s.id
 LIMIT 1`

// tablesStartSQL is the earliest non-empty tables row of the window,
// filtered to the tables named by $2 (schemas) and $3 (names).
var tablesStartSQL = `/* pg_sage */
WITH first AS MATERIALIZED (
    SELECT s.id, s.collected_at FROM sage.snapshots s
     WHERE {db:s} AND s.category = 'tables'
       AND s.collected_at >= now() - make_interval(secs => $1)
       AND ` + snapstore.NonEmptySQL("s") + `
     ORDER BY s.collected_at, s.id LIMIT 1
), doc AS MATERIALIZED (
    SELECT f.collected_at, (SELECT ` + snapstore.DataSQL("s") + ` FROM sage.snapshots s
             WHERE {db:s} AND s.id = f.id AND s.collected_at = f.collected_at) AS d
      FROM first f
)
SELECT doc.collected_at, e->>'schemaname', e->>'relname',
       COALESCE((e->>'n_live_tup')::int8, 0), COALESCE((e->>'n_dead_tup')::int8, 0)
  FROM doc
  LEFT JOIN LATERAL jsonb_array_elements(doc.d) AS t(e)
    ON (e->>'schemaname', e->>'relname') IN (SELECT * FROM unnest($2::text[], $3::text[]))`

// pgWindowHistory reads the window through the database's history store
// (histstore.Resolve: the meta database in history.store: meta).
type pgWindowHistory struct{ db any }

func (p pgWindowHistory) readSystemStart(ctx context.Context, window time.Duration) (
	walStart, bool, error) {
	var at time.Time
	var raw []byte
	err := histstore.Resolve(p.db).QueryRow(ctx, systemStartSQL, window.Seconds()).
		Scan(&at, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return walStart{}, false, nil
	}
	if err != nil {
		return walStart{}, false, err
	}
	if raw == nil { // a delta whose keyframe is gone
		return walStart{}, false, nil
	}
	start := walStart{at: at.UTC()}
	if err := json.Unmarshal(raw, &start.sys); err != nil {
		return walStart{}, false, fmt.Errorf("decode the system row of %s: %w",
			start.at.Format(time.RFC3339), err)
	}
	return start, true, nil
}

func (p pgWindowHistory) readTablesStart(ctx context.Context, window time.Duration,
	keys []tableKey) (tablesStart, bool, error) {
	schemas, names := make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		schemas[i], names[i] = k.schema, k.name
	}
	rows, err := histstore.Resolve(p.db).Query(ctx, tablesStartSQL, window.Seconds(),
		schemas, names)
	if err != nil {
		return tablesStart{}, false, err
	}
	defer rows.Close()
	start := tablesStart{tables: map[tableKey]deadCount{}}
	found := false
	for rows.Next() {
		var at time.Time
		var schema, name *string
		var c deadCount
		if err := rows.Scan(&at, &schema, &name, &c.live, &c.dead); err != nil {
			return tablesStart{}, false, fmt.Errorf("scan the tables row: %w", err)
		}
		start.at, found = at.UTC(), true
		if schema != nil && name != nil {
			start.tables[tableKey{*schema, *name}] = c
		}
	}
	if err := rows.Err(); err != nil {
		return tablesStart{}, false, err
	}
	return start, found, nil
}
