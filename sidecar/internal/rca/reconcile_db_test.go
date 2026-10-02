package rca

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Dogfood lifeos-1: 143 open incidents, all written before identity keys
// existed (identity_key and database_name empty), 13 engine identities.
// Hydration backfills their identity and merges each identity's open
// rows into its earliest one: occurrences summed, last detection the
// latest, severity the worst. Other databases, operator resolutions and
// resolved history are untouched; a second run changes nothing.

// legacyGroup is one engine identity of the fixture.
type legacyGroup struct {
	first   string
	signals string // text[] literal, deliberately unsorted for one group
	rows    int
	pidRows int
}

// lifeosGroups mirrors the 13 identities of lifeos's 143 open incidents,
// plus one multi-signal group with an unsorted signal list and a
// non-ASCII object (identity parity with the Go key).
var lifeosGroups = []legacyGroup{
	{"public.agent_jobs", "{vacuum_blocked}", 36, 2},
	{"public.capabilities", "{vacuum_blocked}", 27, 6},
	{"public.collector_manifests", "{vacuum_blocked}", 26, 2},
	{"public.deliverable_templates", "{vacuum_blocked}", 19, 1},
	{"public.archetype_bandit", "{vacuum_blocked}", 14, 2},
	{"public.attention_rule_proposal", "{vacuum_blocked}", 6, 0},
	{"public.builder_queue", "{vacuum_blocked}", 5, 0},
	{"public.delivery_queue", "{vacuum_blocked}", 4, 0},
	{"public.deliverables", "{vacuum_blocked}", 2, 0},
	{"public.canonical_observation", "{vacuum_blocked}", 1, 1},
	{"test_attention_0a19.capabilities", "{vacuum_blocked}", 1, 0},
	{"test_attention_08ca.attention_rule_proposal", "{vacuum_blocked}", 1, 0},
	{"test_attention_1945.attention_rule_proposal", "{vacuum_blocked}", 1, 0},
	{`public."ünïcode"`, "{wal_growth_spike,checkpoint_storm}", 2, 0},
}

const legacyRows = 145 // sum of lifeosGroups rows

var fixtureBase = time.Date(2026, 6, 3, 1, 0, 0, 0, time.UTC)

// seedLegacy inserts the fixture's open legacy rows (no identity, no
// database), marked by rollback_sql for cleanup.
func seedLegacy(t *testing.T, ctx context.Context, pool *pgxpool.Pool, mark string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE rollback_sql = $1", mark)
	})
	for gi, g := range lifeosGroups {
		_, err := pool.Exec(ctx, `INSERT INTO sage.incidents (detected_at,
			last_detected_at, severity, root_cause, causal_chain, affected_objects,
			signal_ids, source, occurrence_count, database_name, rollback_sql,
			recommended_sql)
		SELECT $1::timestamptz + make_interval(hours => i * 7 + $2),
		       $1::timestamptz + make_interval(hours => i * 7 + $2 + i % 5),
		       CASE WHEN i % 9 = 4 THEN 'critical' ELSE 'warning' END,
		       CASE WHEN i < $6 THEN 'Idle-in-transaction PID ' || (60000 + i) ||
		            ' holding oldest xmin, blocking autovacuum'
		            ELSE 'Autovacuum falling behind -- dead tuple accumulation' END,
		       CASE WHEN i < $6 THEN jsonb_build_array(jsonb_build_object('order', 1,
		            'signal', 'vacuum_blocked', 'evidence',
		            'PID ' || (60000 + i) || ' in state: idle in transaction'))
		            ELSE '[]'::jsonb END,
		       ARRAY[$3::text, 'public.other_' || i],
		       $4::text[], 'deterministic', i % 7 + 1,
		       CASE WHEN i % 2 = 0 THEN '' END, $5, NULL
		FROM generate_series(0, $7 - 1) i`,
			fixtureBase, gi, g.first, g.signals, mark, g.pidRows, g.rows)
		if err != nil {
			t.Fatalf("seed group %s: %v", g.first, err)
		}
	}
}

type groupWant struct {
	detected, last time.Time
	occurrences    int
	severity       string
}

// expectedGroups computes each identity's merged state from the seeded
// rows themselves.
func expectedGroups(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	mark string) map[string]groupWant {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT affected_objects[1], min(detected_at),
		max(last_detected_at), sum(occurrence_count)::int,
		CASE WHEN bool_or(severity = 'critical') THEN 'critical' ELSE 'warning' END
		FROM sage.incidents WHERE rollback_sql = $1 AND resolved_at IS NULL
		GROUP BY 1`, mark)
	if err != nil {
		t.Fatalf("expected groups: %v", err)
	}
	defer rows.Close()
	out := map[string]groupWant{}
	for rows.Next() {
		var k string
		var g groupWant
		if err := rows.Scan(&k, &g.detected, &g.last, &g.occurrences, &g.severity); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[k] = g
	}
	return out
}

type mergedRow struct {
	id, first, key, db, resolvedBy, reason string
	signals                                []string
	resolved                               bool
	detected, last                         time.Time
	occurrences                            int
	severity                               string
}

func fixtureRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	mark string) []mergedRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id::text, affected_objects[1],
		COALESCE(identity_key, ''), COALESCE(database_name, ''),
		COALESCE(resolved_by, ''), COALESCE(resolution_reason, ''), signal_ids,
		resolved_at IS NOT NULL, detected_at, last_detected_at, occurrence_count, severity
		FROM sage.incidents WHERE rollback_sql = $1 ORDER BY id`, mark)
	if err != nil {
		t.Fatalf("fixture rows: %v", err)
	}
	defer rows.Close()
	var out []mergedRow
	for rows.Next() {
		var r mergedRow
		if err := rows.Scan(&r.id, &r.first, &r.key, &r.db, &r.resolvedBy, &r.reason,
			&r.signals, &r.resolved, &r.detected, &r.last, &r.occurrences,
			&r.severity); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func goKey(db string, r mergedRow) string {
	return identityKey(&Incident{Source: "deterministic", DatabaseName: db,
		SignalIDs: r.signals, AffectedObjects: []string{r.first}})
}

func TestReconcile_MergesLifeosLegacyIncidents(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	_, db := lifecycleEngine(t, pool)
	mark := "fixture:" + db
	seedLegacy(t, ctx, pool, mark)
	want := expectedGroups(t, ctx, pool, mark)
	stats, err := ReconcileOpenIncidents(ctx, pool, db)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Backfilled != legacyRows || stats.Merged != legacyRows-int64(len(lifeosGroups)) {
		t.Fatalf("stats = %+v, want %d backfilled and %d merged", stats, legacyRows,
			legacyRows-len(lifeosGroups))
	}
	checkMerged(t, fixtureRows(t, ctx, pool, mark), want, db)
	again, err := ReconcileOpenIncidents(ctx, pool, db)
	if err != nil || again != (ReconcileStats{}) {
		t.Fatalf("second run = %+v (%v), want no changes", again, err)
	}
}

func checkMerged(t *testing.T, rows []mergedRow, want map[string]groupWant, db string) {
	t.Helper()
	survivors := map[string]mergedRow{}
	for _, r := range rows {
		if r.db != db || r.key != goKey(db, r) {
			t.Fatalf("row %s: db %q key %q, want %q and the engine's key", r.id, r.db,
				r.key, db)
		}
		if !r.resolved {
			if _, dup := survivors[r.first]; dup {
				t.Fatalf("two open incidents for %s", r.first)
			}
			survivors[r.first] = r
		}
	}
	if len(survivors) != len(lifeosGroups) {
		t.Fatalf("open incidents = %d, want %d", len(survivors), len(lifeosGroups))
	}
	for first, s := range survivors {
		w := want[first]
		if !s.detected.Equal(w.detected) || !s.last.Equal(w.last) ||
			s.occurrences != w.occurrences || s.severity != w.severity {
			t.Fatalf("%s survivor = %+v, want %+v", first, s, w)
		}
	}
	for _, r := range rows {
		if r.resolved && (r.resolvedBy != ResolvedByMerged ||
			!strings.Contains(r.reason, survivors[r.first].id)) {
			t.Fatalf("merged row %s: by %q reason %q, want merged into %s", r.id,
				r.resolvedBy, r.reason, survivors[r.first].id)
		}
	}
}

// Other databases, operator resolutions and an already-keyed duplicate:
// the keyed later detection merges into the earliest legacy row; the
// rest is untouched.
func TestReconcile_ScopeAndKeyedDuplicates(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	_, db := lifecycleEngine(t, pool)
	mark := "fixture:" + db
	seedLegacy(t, ctx, pool, mark)
	keyedID, otherID, opID := seedScopeRows(t, ctx, pool, db, mark)
	stats, err := ReconcileOpenIncidents(ctx, pool, db)
	if err != nil || stats.Merged != legacyRows+1-int64(len(lifeosGroups)) {
		t.Fatalf("stats = %+v (%v)", stats, err)
	}
	var keyedBy, otherKey, opBy, opKey, otherDB string
	_ = pool.QueryRow(ctx, `SELECT COALESCE(resolved_by, '') FROM sage.incidents
		WHERE id = $1`, keyedID).Scan(&keyedBy)
	_ = pool.QueryRow(ctx, `SELECT COALESCE(identity_key, ''), database_name
		FROM sage.incidents WHERE id = $1`, otherID).Scan(&otherKey, &otherDB)
	_ = pool.QueryRow(ctx, `SELECT resolved_by, COALESCE(identity_key, '')
		FROM sage.incidents WHERE id = $1`, opID).Scan(&opBy, &opKey)
	if keyedBy != ResolvedByMerged {
		t.Errorf("later keyed duplicate resolved_by = %q, want merged", keyedBy)
	}
	if otherKey != "" || otherDB != "other_"+db {
		t.Errorf("another database's row was touched: key %q db %q", otherKey, otherDB)
	}
	if opBy != "user:ops@example.com" || opKey != "" {
		t.Errorf("operator-resolved row touched: by %q key %q", opBy, opKey)
	}
}

// seedScopeRows adds a keyed later duplicate, another database's legacy
// row and an operator-resolved legacy row.
func seedScopeRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db,
	mark string) (keyedID, otherID, opID string) {
	t.Helper()
	keyed := goKey(db, mergedRow{first: "public.agent_jobs",
		signals: []string{"vacuum_blocked"}})
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents (detected_at, severity,
		root_cause, source, signal_ids, affected_objects, database_name, identity_key,
		rollback_sql, occurrence_count)
		VALUES (now(), 'warning', 'new', 'deterministic', '{vacuum_blocked}',
		        '{public.agent_jobs}', $1, $2, $3, 4) RETURNING id::text`,
		db, keyed, mark).Scan(&keyedID); err != nil {
		t.Fatalf("keyed row: %v", err)
	}
	other := "other_" + db
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE database_name = $1", other)
	})
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents (severity, root_cause,
		source, signal_ids, affected_objects, database_name)
		VALUES ('warning', 'x', 'deterministic', '{vacuum_blocked}', '{public.agent_jobs}',
		        $1) RETURNING id::text`, other).Scan(&otherID); err != nil {
		t.Fatalf("other db row: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents (severity, root_cause,
		source, signal_ids, affected_objects, database_name, resolved_at, resolved_by,
		rollback_sql)
		VALUES ('warning', 'x', 'deterministic', '{vacuum_blocked}', '{public.agent_jobs}',
		        '', now(), 'user:ops@example.com', $1) RETURNING id::text`, mark).
		Scan(&opID); err != nil {
		t.Fatalf("operator row: %v", err)
	}
	return keyedID, otherID, opID
}

// Hydration reconciles before loading, and the backfilled key is the
// engine's own: a recurrence links to the merged incident. The other
// merged incidents, last seen in June, resolve as stale without sending
// anyone a notification about a months-old incident.
func TestHydrate_ReconcilesLegacyRowsAndLinksRecurrence(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	rec := &recordingDispatcher{}
	eng.WithDispatcher(rec)
	mark := "fixture:" + db
	seedLegacy(t, ctx, pool, mark)
	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	// Hydrate also adopts legacy rows with no database name, which tests in
	// other packages sharing the server may leave meanwhile; count only this
	// fixture's incidents.
	if n := fixtureActive(t, ctx, pool, eng, mark); n != len(lifeosGroups) {
		t.Fatalf("hydrated %d open fixture incidents, want %d", n, len(lifeosGroups))
	}
	var survivor string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM sage.incidents
		WHERE rollback_sql = $1 AND resolved_at IS NULL
		  AND affected_objects[1] = 'public.agent_jobs'`, mark).Scan(&survivor); err != nil {
		t.Fatalf("survivor: %v", err)
	}
	eng.AnalyzeContext(ctx, deadTupleSnapshot("agent_jobs"), nil, testConfig(), nil)
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("persist: %v", err)
	}
	open := openRows(incidentRows(t, ctx, pool, db))
	if len(open) != 1 || open[0].previousID == nil || *open[0].previousID != survivor {
		t.Fatalf("open = %+v, want one recurrence linked to %s", open, survivor)
	}
	// The survivors whose earliest row named an idle-in-transaction PID (6
	// identities, agent_jobs included: its backend check runs before the
	// recurrence merges) resolve because that backend is gone; the other 8
	// as stale.
	var stale, gone int
	_ = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE resolved_by = $2
		  AND resolution_reason LIKE 'stale:%'),
		count(*) FILTER (WHERE resolved_by = $3)
		FROM sage.incidents WHERE rollback_sql = $1`, mark, ResolvedByStale,
		ResolvedBySubjectGone).Scan(&stale, &gone)
	if stale != 8 || gone != 6 {
		t.Fatalf("%d stale and %d subject-gone resolutions, want 8 and 6", stale, gone)
	}
	if evs := rec.byType("incident_resolved"); len(evs) != 0 {
		t.Fatalf("%d resolution notifications for months-old incidents, want none",
			len(evs))
	}
	if evs := rec.byType("incident_detected"); len(evs) != 1 {
		t.Fatalf("detected notifications = %d, want 1 for the recurrence", len(evs))
	}
}

// Large legacy backlog: thousands of open duplicates over a big resolved
// history are merged in bounded batches.
func TestReconcile_LargeBacklogInBatches(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	_, db := lifecycleEngine(t, pool)
	mark := "fixture:" + db
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE rollback_sql = $1", mark)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.incidents (detected_at, severity,
		root_cause, source, signal_ids, affected_objects, database_name, rollback_sql,
		resolved_at, resolved_by)
		SELECT now() - make_interval(mins => i), 'warning', 'x', 'deterministic',
		       '{vacuum_blocked}', ARRAY['public.big'], $1, $2,
		       CASE WHEN i > 5000 THEN now() END, CASE WHEN i > 5000 THEN 'pg_sage' END
		FROM generate_series(1, 25000) i`, db, mark); err != nil {
		t.Fatalf("seed: %v", err)
	}
	start := time.Now()
	stats, err := ReconcileOpenIncidents(ctx, pool, db)
	if err != nil || stats.Backfilled != 5000 || stats.Merged != 4999 {
		t.Fatalf("stats = %+v (%v), want 5000 backfilled, 4999 merged", stats, err)
	}
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("reconcile took %s", el)
	}
	var open, sum int
	_ = pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(occurrence_count), 0)::int
		FROM sage.incidents WHERE rollback_sql = $1 AND resolved_at IS NULL`, mark).
		Scan(&open, &sum)
	if open != 1 || sum != 5000 {
		t.Fatalf("open %d with %d occurrences, want 1 with 5000", open, sum)
	}
}

// Concurrent sidecars hydrating the same store merge each row once.
func TestReconcile_ConcurrentRunsMergeOnce(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	_, db := lifecycleEngine(t, pool)
	mark := "fixture:" + db
	seedLegacy(t, ctx, pool, mark)
	want := expectedGroups(t, ctx, pool, mark)
	var mu sync.Mutex
	var merged int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := ReconcileOpenIncidents(ctx, pool, db)
			if err != nil {
				t.Errorf("concurrent reconcile: %v", err)
			}
			mu.Lock()
			merged += s.Merged
			mu.Unlock()
		}()
	}
	wg.Wait()
	if merged != legacyRows-int64(len(lifeosGroups)) {
		t.Fatalf("merged %d in total, want %d", merged, legacyRows-len(lifeosGroups))
	}
	checkMerged(t, fixtureRows(t, ctx, pool, mark), want, db)
}

// Invalid input and error propagation.
func TestReconcile_InvalidInputAndErrors(t *testing.T) {
	if _, err := ReconcileOpenIncidents(context.Background(), nil, "db"); err == nil ||
		!strings.Contains(err.Error(), "pool") {
		t.Fatalf("nil pool err = %v", err)
	}
	pool, _ := lifecycleDB(t)
	if _, err := ReconcileOpenIncidents(context.Background(), pool, ""); err == nil ||
		!strings.Contains(err.Error(), "database") {
		t.Fatalf("empty database err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ReconcileOpenIncidents(ctx, pool, "db")
	if err == nil || !strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("canceled err = %v, want a reconcile error", err)
	}
	_ = fmt.Sprint(err)
}

// deadTupleSnapshot fires vacuum_blocked for public.<table> only.
func deadTupleSnapshot(table string) *collector.Snapshot {
	snap := quietSnapshot()
	snap.Tables = []collector.TableStats{{SchemaName: "public", RelName: table,
		NLiveTup: 100, NDeadTup: 900}}
	return snap
}

// fixtureActive counts the engine's open incidents that are rows of the
// fixture marked by mark.
func fixtureActive(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, eng *Engine, mark string,
) int {
	t.Helper()
	rows, err := pool.Query(ctx,
		"SELECT id::text FROM sage.incidents WHERE rollback_sql = $1", mark)
	if err != nil {
		t.Fatalf("fixture ids: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect fixture ids: %v", err)
	}
	own := make(map[string]bool, len(ids))
	for _, id := range ids {
		own[id] = true
	}
	n := 0
	for _, inc := range eng.ActiveIncidents() {
		if own[inc.ID] {
			n++
		}
	}
	return n
}
