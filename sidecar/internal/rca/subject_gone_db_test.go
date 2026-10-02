package rca

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Dogfood lifeos-1, against real PostgreSQL: "Idle-in-transaction PID
// 6686 holding oldest xmin" stayed open for months after PID 6686 was
// gone. A backend-subject incident resolves once pg_stat_activity no
// longer has that backend (pid and backend_start); a live one stays.

// holdBackend opens a session idle in a transaction and returns its pid
// and backend_start, and a func that ends it.
func holdBackend(t *testing.T, ctx context.Context) (int, time.Time, func()) {
	t.Helper()
	conn, err := pgx.Connect(ctx, os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var pid int
	var start time.Time
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid(), backend_start
		FROM pg_stat_activity WHERE pid = pg_backend_pid()`).Scan(&pid, &start); err != nil {
		t.Fatalf("backend identity: %v", err)
	}
	if _, err := conn.Exec(ctx, "BEGIN; SELECT txid_current()"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	closed := false
	end := func() {
		if !closed {
			closed = true
			_ = conn.Close(context.Background())
		}
	}
	t.Cleanup(end)
	return pid, start, end
}

func waitGone(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE pid = $1`, pid).Scan(&n); err == nil && n == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("backend %d still listed", pid)
}

// seedHolderIncident stores an open vacuum_blocked incident naming pid
// (and backend_start when not zero), last detected lastSeen ago.
func seedHolderIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string,
	pid int, start time.Time, first string, lastSeen time.Duration) string {
	t.Helper()
	link := map[string]any{"order": 1, "signal": "vacuum_blocked",
		"evidence": "PID " + strconv.Itoa(pid) + " in state: idle in transaction"}
	if !start.IsZero() {
		link["blocker"] = map[string]any{"pid": pid, "backend_start": start}
	}
	var id string
	err := pool.QueryRow(ctx, `INSERT INTO sage.incidents (detected_at, last_detected_at,
		severity, root_cause, causal_chain, source, signal_ids, affected_objects,
		database_name)
		VALUES (now() - make_interval(secs => $1), now() - make_interval(secs => $1), 'warning',
		        'Idle-in-transaction PID ' || $2::text || ' holding oldest xmin',
		        jsonb_build_array($3::jsonb), 'deterministic', '{vacuum_blocked}',
		        ARRAY[$4::text], $5) RETURNING id::text`,
		lastSeen.Seconds(), strconv.Itoa(pid), link, first, db).Scan(&id)
	if err != nil {
		t.Fatalf("seed holder incident: %v", err)
	}
	return id
}

func resolution(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	id string) (by, reason string) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT COALESCE(resolved_by, ''),
		COALESCE(resolution_reason, '') FROM sage.incidents WHERE id = $1`, id).
		Scan(&by, &reason); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return by, reason
}

func TestSubjectGone_ResolvesWhenTheBackendExits(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	rec := &recordingDispatcher{}
	eng.WithDispatcher(rec)
	pid, start, end := holdBackend(t, ctx)
	id := seedHolderIncident(t, ctx, pool, db, pid, start, "public.held", 5*time.Minute)
	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	cycle(t, ctx, eng, pool, false)
	if by, _ := resolution(t, ctx, pool, id); by != "" {
		t.Fatalf("resolved by %q while backend %d is alive", by, pid)
	}
	end()
	waitGone(t, ctx, pool, pid)
	cycle(t, ctx, eng, pool, false)
	by, reason := resolution(t, ctx, pool, id)
	if by != ResolvedBySubjectGone || !strings.Contains(reason, strconv.Itoa(pid)) {
		t.Fatalf("resolution = %q / %q, want subject gone naming pid %d", by, reason, pid)
	}
	evs := rec.byType("incident_resolved")
	if len(evs) != 1 || evs[0].Data["resolved_by"] != ResolvedBySubjectGone {
		t.Fatalf("resolution events = %+v, want one for a live incident", evs)
	}
}

// Legacy rows carry the pid only: a pid that no longer exists resolves; a
// pid that exists now (maybe reused) stays open for the stale window.
// A pid with a different backend_start was reused: resolved.
func TestSubjectGone_LegacyAndReusedPIDs(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	pid, start, _ := holdBackend(t, ctx)
	gone := seedHolderIncident(t, ctx, pool, db, 99999999, time.Time{}, "public.g",
		5*time.Minute)
	alive := seedHolderIncident(t, ctx, pool, db, pid, time.Time{}, "public.a",
		5*time.Minute)
	reused := seedHolderIncident(t, ctx, pool, db, pid, start.Add(-time.Hour), "public.r",
		5*time.Minute)
	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	cycle(t, ctx, eng, pool, false)
	if by, _ := resolution(t, ctx, pool, gone); by != ResolvedBySubjectGone {
		t.Errorf("legacy row of a gone pid resolved by %q", by)
	}
	if by, _ := resolution(t, ctx, pool, alive); by != "" {
		t.Errorf("legacy row of a live pid resolved by %q", by)
	}
	if by, _ := resolution(t, ctx, pool, reused); by != ResolvedBySubjectGone {
		t.Errorf("row of a reused pid resolved by %q", by)
	}
}

// An operator who resolved the incident keeps the resolution; the engine
// never overwrites it with its own.
func TestSubjectGone_OperatorResolutionIsKept(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	id := seedHolderIncident(t, ctx, pool, db, 99999999, time.Time{}, "public.o",
		5*time.Minute)
	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	if err := ResolveIncident(ctx, pool, id, "user:ops@example.com", "killed it"); err != nil {
		t.Fatalf("operator resolve: %v", err)
	}
	cycle(t, ctx, eng, pool, false)
	if by, reason := resolution(t, ctx, pool, id); by != "user:ops@example.com" ||
		reason != "killed it" {
		t.Fatalf("resolution = %q / %q, want the operator's", by, reason)
	}
}
