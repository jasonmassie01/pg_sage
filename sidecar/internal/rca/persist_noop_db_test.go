package rca

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Perf storage phase (dogfood lifeos): 3,406 incident updates for 4 open
// incidents. Every persist pass (analyzer cycle, lock fast path, detector
// episodes) rewrote every tracked incident, including its 2.8-45 kB TOASTed
// causal chain, changed or not.

func incidentXmin(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var x string
	if err := pool.QueryRow(ctx, `SELECT xmin::text FROM sage.incidents WHERE id = $1`, id).
		Scan(&x); err != nil {
		t.Fatalf("xmin of %s: %v", id, err)
	}
	return x
}

func TestPersist_UnchangedIncidentIsNotRewritten(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	cycle(t, ctx, eng, pool, true)
	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	before := incidentXmin(t, ctx, pool, rows[0].id)
	for i := 0; i < 3; i++ { // passes with no new detection in between
		if err := eng.PersistIncidents(ctx, pool); err != nil {
			t.Fatalf("PersistIncidents: %v", err)
		}
	}
	if after := incidentXmin(t, ctx, pool, rows[0].id); after != before {
		t.Fatalf("unchanged incident rewritten (xmin %s -> %s)", before, after)
	}
}

// Skipping unchanged incidents must not hide an operator's resolution: a
// pass with nothing to write still adopts it (R04).
func TestPersist_ExternalResolutionIsAdoptedWithoutAWrite(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	cycle(t, ctx, eng, pool, true)
	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 1 || len(eng.ActiveIncidents()) != 1 {
		t.Fatalf("rows = %+v, active = %d", rows, len(eng.ActiveIncidents()))
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.incidents SET resolved_at = now(),
		resolved_by = 'user:ops@example.com', resolution_reason = 'fixed by hand'
		WHERE id = $1`, rows[0].id); err != nil {
		t.Fatal(err)
	}
	resolvedXmin := incidentXmin(t, ctx, pool, rows[0].id)
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents: %v", err)
	}
	if n := len(eng.ActiveIncidents()); n != 0 {
		t.Fatalf("engine still tracks %d open incidents after an external resolution", n)
	}
	if after := incidentXmin(t, ctx, pool, rows[0].id); after != resolvedXmin {
		t.Fatal("adopting an external resolution rewrote the row")
	}
	got := incidentRows(t, ctx, pool, db)
	if got[0].resolvedBy == nil || *got[0].resolvedBy != "user:ops@example.com" {
		t.Fatalf("resolved_by = %v", got[0].resolvedBy)
	}
}

// A row deleted outside the engine is dropped from memory, as before.
func TestPersist_ExternallyDeletedIncidentLeavesMemory(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	cycle(t, ctx, eng, pool, true)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.incidents WHERE database_name = $1`,
		db); err != nil {
		t.Fatal(err)
	}
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents: %v", err)
	}
	if n := len(eng.ActiveIncidents()); n != 0 {
		t.Fatalf("engine tracks %d incidents whose rows were deleted", n)
	}
}

// When only last_detected_at moves, the causal chain keeps its TOAST
// pointer: no new TOAST chunks are written.
func TestPersist_UnchangedCausalChainWritesNoToast(t *testing.T) {
	shared, ctx := lifecycleDB(t)
	testdb.RequireServerVersion(t, shared, 150000, "pg_stat_force_next_flush")
	cfg := shared.Config().Copy()
	cfg.MaxConns, cfg.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, db := lifecycleEngine(t, shared)
	inc := bigChainIncident(db)
	if _, err := persistOne(ctx, pool, persistItem{inc: inc}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	before := toastInserts(t, ctx, pool)
	inc.LastDetectedAt = inc.LastDetectedAt.Add(time.Minute)
	inc.OccurrenceCount++
	if _, err := persistOne(ctx, pool, persistItem{inc: inc, persisted: true}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := toastInserts(t, ctx, pool) - before; got != 0 {
		t.Fatalf("refreshing last_detected_at wrote %d TOAST chunks", got)
	}
	var occ int
	if err := pool.QueryRow(ctx, `SELECT occurrence_count FROM sage.incidents WHERE id = $1`,
		inc.ID).Scan(&occ); err != nil || occ != inc.OccurrenceCount {
		t.Fatalf("occurrence_count = %d, %v; the update did not land", occ, err)
	}
	// A changed chain is written.
	inc.CausalChain[0].Evidence = strings.Repeat("y", 9000)
	if _, err := persistOne(ctx, pool, persistItem{inc: inc, persisted: true}); err != nil {
		t.Fatalf("update chain: %v", err)
	}
	if got := toastInserts(t, ctx, pool) - before; got == 0 {
		t.Fatal("a changed causal chain wrote no TOAST")
	}
}

// bigChainIncident is an open incident whose causal chain is TOASTed
// (well over the 2 kB inline limit, and not compressible below it).
func bigChainIncident(db string) Incident {
	now := time.Now().UTC().Truncate(time.Microsecond)
	var sb strings.Builder
	for i := 0; sb.Len() < 12000; i++ {
		sb.WriteString(time.Duration(i * 7919).String())
	}
	return Incident{ID: newUUID(), DetectedAt: now, LastDetectedAt: now,
		Severity: "warning", RootCause: "toast test", Source: "deterministic",
		DatabaseName: db, OccurrenceCount: 1, SignalIDs: []string{"toast_test"},
		CausalChain: []ChainLink{{Order: 1, Signal: "toast_test", Evidence: sb.String()}}}
}

func toastInserts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	if _, err := pool.Exec(ctx, "SELECT pg_stat_force_next_flush()"); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(s.n_tup_ins, 0) FROM pg_class c
		JOIN pg_stat_all_tables s ON s.relid = c.reltoastrelid
		WHERE c.oid = 'sage.incidents'::regclass`).Scan(&n); err != nil {
		t.Fatalf("toast counters: %v", err)
	}
	return n
}
