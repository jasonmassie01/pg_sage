package rca

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/signalid"
)

// rca-07 against the per-package fixture database: a periodic log event
// is one incident, a recurrence soon after resolution is not paged again,
// and the occurrences are stored without risking the incident row.

// flapRig drives an engine on a fake clock with a log source the test fills.
type flapRig struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	eng  *Engine
	db   string
	src  *mockLogSource
	base time.Time
	now  time.Time
}

func newFlapRig(t *testing.T) *flapRig {
	t.Helper()
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	r := &flapRig{t: t, ctx: ctx, pool: pool, eng: eng, db: db,
		src: &mockLogSource{}, base: time.Now()}
	r.now = r.base
	eng.now = func() time.Time { return r.now }
	eng.SetLogSource(r.src)
	return r
}

// at runs one analyzer cycle at base+offset, with a deadlock line first
// when deadlock is set.
func (r *flapRig) at(offset time.Duration, deadlock bool) {
	r.t.Helper()
	r.now = r.base.Add(offset)
	if deadlock {
		r.src.signals = append(r.src.signals, &Signal{
			ID: signalid.LogDeadlockDetected, FiredAt: r.now, Severity: "critical",
			Metrics: map[string]any{"pid": 4242, "occurrence_count": 1,
				"affected_pids": []int{4242, 4243},
				"first_seen":    r.now.Format(time.RFC3339Nano),
				"last_seen":     r.now.Format(time.RFC3339Nano)}})
	}
	cycle(r.t, r.ctx, r.eng, r.pool, false)
}

func (r *flapRig) storedCount(id string) (count int, null bool) {
	r.t.Helper()
	var n *int
	err := r.pool.QueryRow(r.ctx, `SELECT (log_occurrences->>'count')::int
		FROM sage.incidents WHERE id = $1`, id).Scan(&n)
	if err != nil {
		r.t.Fatalf("read log_occurrences of %s: %v", id, err)
	}
	if n == nil {
		return 0, true
	}
	return *n, false
}

// State transition: a deadlock every 15 min for 2 h is one open incident
// and one incident_detected; a recurrence 2 h after its resolution is
// linked and not paged, and one 7 h after the next resolution is paged.
func TestLifecycle_PeriodicDeadlockDoesNotFlap(t *testing.T) {
	r := newFlapRig(t)
	rec := &recordingDispatcher{}
	r.eng.WithDispatcher(rec)
	for m := 0; m <= 120; m += 5 {
		r.at(time.Duration(m)*time.Minute, m%15 == 0)
	}
	rows := incidentRows(t, r.ctx, r.pool, r.db)
	if len(rows) != 1 || len(openRows(rows)) != 1 {
		t.Fatalf("rows after 2 h = %+v, want one open incident", rows)
	}
	first := rows[0].id
	if n := len(rec.byType("incident_detected")); n != 1 {
		t.Fatalf("incident_detected = %d, want 1", n)
	}
	if n, null := r.storedCount(first); null || n != 9 {
		t.Errorf("stored log_occurrences count = %d (null=%v), want 9", n, null)
	}
	for m := 125; m <= 150; m += 5 { // the 30 min quiet period ends at 150
		r.at(time.Duration(m)*time.Minute, false)
	}
	rows = incidentRows(t, r.ctx, r.pool, r.db)
	if len(rows) != 1 || rows[0].resolvedAt == nil {
		t.Fatalf("not resolved after the quiet period: %+v", rows)
	}
	if n := len(rec.byType("incident_resolved")); n != 1 {
		t.Errorf("incident_resolved = %d, want 1", n)
	}

	r.at(270*time.Minute, true) // 2 h after the resolution
	second := assertRecurrence(t, r, first, 2)
	if n := len(rec.byType("incident_detected")); n != 1 {
		t.Errorf("recurrence 2 h after resolution was paged: detected = %d", n)
	}
	r.at(300*time.Minute, false) // quiet period of the recurrence ends
	if n := len(rec.byType("incident_resolved")); n != 1 {
		t.Errorf("silent recurrence's resolution was paged: resolved = %d", n)
	}

	r.at(720*time.Minute, true) // 7 h after the second resolution
	assertRecurrence(t, r, second, 3)
	if n := len(rec.byType("incident_detected")); n != 2 {
		t.Errorf("recurrence 7 h after resolution: detected = %d, want 2", n)
	}
}

// assertRecurrence checks the newest row is open and linked to prev, and
// returns its id.
func assertRecurrence(t *testing.T, r *flapRig, prev string, wantRows int) string {
	t.Helper()
	rows := incidentRows(t, r.ctx, r.pool, r.db)
	if len(rows) != wantRows {
		t.Fatalf("rows = %d, want %d: %+v", len(rows), wantRows, rows)
	}
	open := openRows(rows)
	if len(open) != 1 {
		t.Fatalf("open rows = %+v, want one recurrence", open)
	}
	if open[0].previousID == nil || *open[0].previousID != prev {
		t.Fatalf("recurrence previous_incident_id = %v, want %s", open[0].previousID, prev)
	}
	return open[0].id
}

// Error propagation: when the log_occurrences write fails, the incident
// row stays, the error is logged with the incident id, and the next pass
// writes the occurrences.
func TestPersist_LogOccurrencesFailureKeepsIncidentRow(t *testing.T) {
	r := newFlapRig(t)
	var mu sync.Mutex
	var lines []string
	r.eng.logFn = func(level, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, level+" "+fmt.Sprintf(msg, args...))
	}
	// r.db is built from the test name: lowercase letters and underscores.
	constraint := `ALTER TABLE sage.incidents ADD CONSTRAINT rca07_occ_fail
		CHECK (log_occurrences IS NULL OR database_name <> '` + r.db + `') NOT VALID`
	if _, err := r.pool.Exec(r.ctx, constraint); err != nil {
		t.Fatalf("add failing constraint: %v", err)
	}
	drop := func() {
		_, err := r.pool.Exec(context.Background(),
			`ALTER TABLE sage.incidents DROP CONSTRAINT IF EXISTS rca07_occ_fail`)
		if err != nil {
			t.Errorf("drop constraint: %v", err)
		}
	}
	t.Cleanup(drop)

	r.at(0, true)
	rows := incidentRows(t, r.ctx, r.pool, r.db)
	if len(rows) != 1 {
		t.Fatalf("incident row lost with the occurrences write: %+v", rows)
	}
	id := rows[0].id
	if _, null := r.storedCount(id); !null {
		t.Error("log_occurrences written despite the failing constraint")
	}
	mu.Lock()
	logged := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(logged, id) || !strings.Contains(logged, "log_occurrences") ||
		!strings.Contains(logged, "rca07_occ_fail") {
		t.Errorf("log lacks the incident id and the cause:\n%s", logged)
	}
	drop()
	r.at(time.Minute, false)
	if n, null := r.storedCount(id); null || n != 1 {
		t.Errorf("retry stored count = %d (null=%v), want 1", n, null)
	}
}

// Integration: occurrences survive a restart (hydration), and a later
// window adds to the stored count instead of replacing it.
func TestHydrate_RestoresLogOccurrences(t *testing.T) {
	r := newFlapRig(t)
	r.at(0, true)
	rows := incidentRows(t, r.ctx, r.pool, r.db)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	eng2 := testEngine()
	eng2.WithDatabaseName(r.db)
	if err := eng2.Hydrate(r.ctx, r.pool); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	got := eng2.ActiveIncidents()
	if len(got) != 1 || got[0].LogOccurrences == nil || got[0].LogOccurrences.Count != 1 {
		t.Fatalf("hydrated occurrences = %+v", got)
	}
	occ := got[0].LogOccurrences
	if len(occ.PIDs) != 2 || occ.PIDs[0] != 4242 || occ.PIDs[1] != 4243 {
		t.Errorf("hydrated pids = %v, want [4242 4243]", occ.PIDs)
	}
	r.eng, r.src = eng2, &mockLogSource{}
	eng2.now = func() time.Time { return r.now }
	eng2.SetLogSource(r.src)
	r.at(5*time.Minute, true)
	if n, _ := r.storedCount(rows[0].id); n != 2 {
		t.Errorf("count after a second window = %d, want 2", n)
	}
}

// Migration: the column is nullable jsonb and bootstrap is idempotent.
func TestSchema_IncidentsLogOccurrencesColumn(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	var typ, nullable string
	err := pool.QueryRow(ctx, `SELECT data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'incidents'
		  AND column_name = 'log_occurrences'`).Scan(&typ, &nullable)
	if err != nil {
		t.Fatalf("log_occurrences column: %v", err)
	}
	if typ != "jsonb" || nullable != "YES" {
		t.Errorf("log_occurrences = %s nullable=%s, want nullable jsonb", typ, nullable)
	}
}
