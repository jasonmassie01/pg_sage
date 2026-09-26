package rca

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

// Regression tests for R04 / substrate-B4, B5, B7 and SURF-19: incident
// state must have one durable owner. They run against the per-package
// fixture database created by testdb.Run.

var (
	lcPool     *pgxpool.Pool
	lcPoolOnce sync.Once
	lcPoolErr  error
)

func lifecycleDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	lcPoolOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		pool, err := pgxpool.New(ctx, os.Getenv("SAGE_TEST_DATABASE_URL"))
		if err != nil {
			lcPoolErr = err
			return
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			lcPoolErr = err
			return
		}
		if err := schema.Bootstrap(ctx, pool); err != nil {
			pool.Close()
			lcPoolErr = err
			return
		}
		lcPool = pool
	})
	if lcPoolErr != nil {
		t.Skipf("SKIP: fixture database unavailable: %v", lcPoolErr)
	}
	return lcPool, context.Background()
}

// lifecycleEngine returns an engine bound to a database name unique to the
// test, and removes that name's rows afterwards.
func lifecycleEngine(
	t *testing.T, pool *pgxpool.Pool,
) (*Engine, string) {
	t.Helper()
	db := "lc_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	clean := func() {
		_, err := pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE database_name = $1", db)
		if err != nil {
			t.Errorf("cleanup incidents for %s: %v", db, err)
		}
	}
	clean()
	t.Cleanup(clean)
	eng := testEngine()
	eng.WithDatabaseName(db)
	return eng, db
}

func cycle(
	t *testing.T, ctx context.Context, eng *Engine, pool *pgxpool.Pool,
	hot bool,
) {
	t.Helper()
	snap := quietSnapshot()
	if hot {
		snap = hotSnapshotForRegress()
	}
	eng.AnalyzeContext(ctx, snap, nil, testConfig(), nil)
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents: %v", err)
	}
}

type incidentDBRow struct {
	id          string
	resolvedAt  *time.Time
	resolvedBy  *string
	reason      *string
	previousID  *string
	occurrences int
	dbName      *string
}

func incidentRows(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string,
) []incidentDBRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id::text, resolved_at,
		resolved_by, resolution_reason, previous_incident_id::text,
		occurrence_count, database_name
		FROM sage.incidents WHERE database_name = $1
		ORDER BY detected_at, id`, db)
	if err != nil {
		t.Fatalf("query incidents: %v", err)
	}
	defer rows.Close()
	var out []incidentDBRow
	for rows.Next() {
		var r incidentDBRow
		if err := rows.Scan(&r.id, &r.resolvedAt, &r.resolvedBy,
			&r.reason, &r.previousID, &r.occurrences, &r.dbName); err != nil {
			t.Fatalf("scan incident: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate incidents: %v", err)
	}
	return out
}

func openRows(rows []incidentDBRow) []incidentDBRow {
	var out []incidentDBRow
	for _, r := range rows {
		if r.resolvedAt == nil {
			out = append(out, r)
		}
	}
	return out
}

func TestLifecycle_ManualResolveSurvivesNextCycle(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	cycle(t, ctx, eng, pool, true)
	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 1 {
		t.Fatalf("rows after first cycle = %d, want 1", len(rows))
	}
	first := rows[0].id
	if _, err := pool.Exec(ctx, `UPDATE sage.incidents
		SET resolved_at = now(), resolved_by = 'user:ops@example.com',
		    resolution_reason = 'pool restarted'
		WHERE id = $1`, first); err != nil {
		t.Fatalf("manual resolve: %v", err)
	}

	cycle(t, ctx, eng, pool, true) // condition still present
	rows = incidentRows(t, ctx, pool, db)
	var resolved *incidentDBRow
	for i := range rows {
		if rows[i].id == first {
			resolved = &rows[i]
		}
	}
	if resolved == nil || resolved.resolvedAt == nil {
		t.Fatalf("manual resolution of %s was overwritten: %+v",
			first, rows)
	}
	if resolved.resolvedBy == nil ||
		*resolved.resolvedBy != "user:ops@example.com" {
		t.Errorf("resolved_by = %v, want user:ops@example.com",
			resolved.resolvedBy)
	}
	if resolved.reason == nil || *resolved.reason != "pool restarted" {
		t.Errorf("resolution_reason = %v, want pool restarted",
			resolved.reason)
	}
	open := openRows(rows)
	if len(open) != 1 {
		t.Fatalf("open rows = %d, want 1 recurrence: %+v", len(open), rows)
	}
	if open[0].id == first {
		t.Fatal("recurrence silently reopened the resolved incident")
	}
	if open[0].previousID == nil || *open[0].previousID != first {
		t.Errorf("recurrence previous_incident_id = %v, want %s",
			open[0].previousID, first)
	}
}

func TestLifecycle_HydrateKeepsIncidentIdentityAcrossRestart(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng1, db := lifecycleEngine(t, pool)
	if err := eng1.Hydrate(ctx, pool); err != nil {
		t.Fatalf("Hydrate eng1: %v", err)
	}
	cycle(t, ctx, eng1, pool, true)
	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}

	eng2 := testEngine() // simulated restart
	eng2.WithDatabaseName(db)
	if err := eng2.Hydrate(ctx, pool); err != nil {
		t.Fatalf("Hydrate eng2: %v", err)
	}
	if n := len(eng2.ActiveIncidents()); n != 1 {
		t.Fatalf("hydrated active incidents = %d, want 1", n)
	}
	cycle(t, ctx, eng2, pool, true)

	after := incidentRows(t, ctx, pool, db)
	if len(after) != 1 {
		t.Fatalf("rows after restart = %d, want 1 (same identity): %+v",
			len(after), after)
	}
	if after[0].id != rows[0].id {
		t.Errorf("incident id changed across restart: %s -> %s",
			rows[0].id, after[0].id)
	}
	if after[0].occurrences != 2 {
		t.Errorf("occurrence_count = %d, want 2", after[0].occurrences)
	}
}

func TestLifecycle_HydrateAdoptsLegacyRowsWithoutDatabase(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	var id string
	err := pool.QueryRow(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, source, signal_ids, affected_objects,
		 database_name)
		VALUES ('warning', 'legacy', 'deterministic',
		        '{connections_high}', '{pg_stat_activity}', '')
		RETURNING id::text`).Scan(&id)
	if err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE id = $1", id)
	})

	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	cycle(t, ctx, eng, pool, true)
	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 1 || rows[0].id != id {
		t.Fatalf("legacy row not adopted: rows=%+v legacy=%s", rows, id)
	}
}

func TestLifecycle_ResolvedIncidentsLeaveMemory(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)

	cycle(t, ctx, eng, pool, true)
	for i := 0; i < 6; i++ {
		cycle(t, ctx, eng, pool, false)
	}
	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 1 || rows[0].resolvedAt == nil {
		t.Fatalf("incident not auto-resolved in DB: %+v", rows)
	}
	if rows[0].resolvedBy == nil || *rows[0].resolvedBy != ResolvedByAuto {
		t.Errorf("resolved_by = %v, want %s", rows[0].resolvedBy,
			ResolvedByAuto)
	}
	if rows[0].reason == nil || !strings.Contains(*rows[0].reason, "clear") {
		t.Errorf("resolution_reason = %v, want mention of cleared signals",
			rows[0].reason)
	}
	eng.mu.Lock()
	n := len(eng.incidents)
	eng.mu.Unlock()
	if n != 0 {
		t.Errorf("in-memory incidents after durable resolve = %d, want 0", n)
	}
}

func TestResolveIncident_PersistsActorReasonAndErrors(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	cycle(t, ctx, eng, pool, true)
	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	id := rows[0].id

	err := ResolveIncident(ctx, pool, id, "user:ops@example.com", "fixed")
	if err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}
	rows = incidentRows(t, ctx, pool, db)
	if rows[0].resolvedAt == nil || rows[0].resolvedBy == nil ||
		*rows[0].resolvedBy != "user:ops@example.com" ||
		rows[0].reason == nil || *rows[0].reason != "fixed" {
		t.Fatalf("resolution not persisted: %+v", rows[0])
	}

	err = ResolveIncident(ctx, pool, id, "user:x", "again")
	if !errors.Is(err, ErrIncidentAlreadyResolved) {
		t.Errorf("second resolve err = %v, want ErrIncidentAlreadyResolved",
			err)
	}
	err = ResolveIncident(ctx, pool,
		"00000000-0000-4000-8000-000000000000", "user:x", "")
	if !errors.Is(err, ErrIncidentNotFound) {
		t.Errorf("unknown id err = %v, want ErrIncidentNotFound", err)
	}
	err = ResolveIncident(ctx, pool, "not-a-uuid", "user:x", "")
	if !errors.Is(err, ErrIncidentNotFound) {
		t.Errorf("malformed id err = %v, want ErrIncidentNotFound", err)
	}
}

func TestPruneResolvedIncidents_DeletesOnlyOldResolved(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	_, db := lifecycleEngine(t, pool)
	seed := func(resolvedAgo string) string {
		var id string
		err := pool.QueryRow(ctx, `INSERT INTO sage.incidents
			(severity, root_cause, source, database_name, resolved_at)
			VALUES ('warning', 'seed', 'deterministic', $1,
			        CASE WHEN $2 = '' THEN NULL
			             ELSE now() - $2::interval END)
			RETURNING id::text`, db, resolvedAgo).Scan(&id)
		if err != nil {
			t.Fatalf("seed incident: %v", err)
		}
		return id
	}
	old := seed("100 days")
	recent := seed("1 day")
	open := seed("")
	if _, err := pool.Exec(ctx, `UPDATE sage.incidents
		SET previous_incident_id = $1 WHERE id = $2`, old, recent); err != nil {
		t.Fatalf("link recurrence: %v", err)
	}

	n, err := PruneResolvedIncidents(ctx, pool, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("PruneResolvedIncidents: %v", err)
	}
	if n < 1 {
		t.Errorf("deleted = %d, want >= 1", n)
	}
	rows := incidentRows(t, ctx, pool, db)
	ids := map[string]incidentDBRow{}
	for _, r := range rows {
		ids[r.id] = r
	}
	if _, ok := ids[old]; ok {
		t.Error("100-day-old resolved incident was not pruned")
	}
	if r, ok := ids[recent]; !ok || r.previousID != nil {
		t.Errorf("recent resolved row missing or link not cleared: %+v", r)
	}
	if _, ok := ids[open]; !ok {
		t.Error("open incident was pruned")
	}
	if _, err := PruneResolvedIncidents(ctx, pool, 0); err == nil {
		t.Error("zero retention must be rejected, not delete everything")
	}
}
