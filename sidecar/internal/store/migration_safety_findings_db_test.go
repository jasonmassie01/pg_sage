package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/migration"
)

func migrationUpsertInput(ident string) migration.MigrationSafetyFinding {
	return migration.MigrationSafetyFinding{
		RuleID:           "ddl_vacuum_full",
		Severity:         "warning",
		ObjectType:       "migration",
		ObjectIdentifier: ident,
		Title:            "Dangerous DDL: ddl_vacuum_full",
		Detail:           map[string]any{"risk_score": 0.6},
		Recommendation:   "Use regular VACUUM instead",
		RecommendedSQL:   "VACUUM orders",
		ImpactScore:      0.6,
	}
}

func cleanupMigrationIdent(t *testing.T, ident string) {
	t.Helper()
	pool, _ := requireMigrationSafetyDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.findings WHERE object_identifier = $1`, ident)
	})
}

type migrationRowState struct {
	rows, occurrences int
	severity, title   string
}

func readMigrationRows(
	t *testing.T, ctx context.Context, ident, status string,
) migrationRowState {
	t.Helper()
	pool, _ := requireMigrationSafetyDB(t)
	var s migrationRowState
	err := pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(max(occurrence_count), 0),
		        COALESCE(max(severity), ''), COALESCE(max(title), '')
		   FROM sage.findings
		  WHERE category = $1 AND object_identifier = $2 AND status = $3`,
		migration.SafetyFindingCategory, ident, status,
	).Scan(&s.rows, &s.occurrences, &s.severity, &s.title)
	if err != nil {
		t.Fatalf("read migration findings: %v", err)
	}
	return s
}

// Concurrency (migration-12): the activity poller and the log detector
// can upsert the same finding at once. A select-then-insert lets both
// miss the open row; one then fails with 23505 and the occurrence is
// lost. Twenty parallel upserts must leave one row counted twenty times.
func TestMigrationSafetyUpsertTwentyParallelKeepsEveryOccurrence(t *testing.T) {
	const workers = 20
	shared, ctx := requireMigrationSafetyDB(t)
	pool := warmPool(t, ctx, shared.Config().ConnString(), workers)
	ident := fmt.Sprintf("public.orders:race:%d", time.Now().UnixNano())
	cleanupMigrationIdent(t, ident)
	store := NewMigrationSafetyFindingStore(pool)

	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make([]int64, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i], errs[i] = store.UpsertMigrationSafetyFinding(
				ctx, migrationUpsertInput(ident))
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("upsert %d: %v", i, err)
		}
	}
	for i, id := range ids {
		if id <= 0 || id != ids[0] {
			t.Errorf("upsert %d returned id %d, want every id = %d (> 0)", i, id, ids[0])
		}
	}
	got := readMigrationRows(t, ctx, ident, "open")
	if got.rows != 1 || got.occurrences != workers {
		t.Fatalf("open rows=%d occurrence_count=%d, want 1 and %d",
			got.rows, got.occurrences, workers)
	}
}

// warmPool opens a pool of n connections that are all established before
// the test starts, so parallel statements really overlap instead of
// queueing behind connection setup.
func warmPool(t *testing.T, ctx context.Context, dsn string, n int) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns, cfg.MinConns = int32(n), int32(n)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	conns := make([]*pgxpool.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("warm connection %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
	return pool
}

// State transition: a resolved finding does not absorb a new detection;
// the next upsert opens a fresh row counted once, and the resolved row
// keeps its own count.
func TestMigrationSafetyUpsertAfterResolveOpensNewRow(t *testing.T) {
	pool, ctx := requireMigrationSafetyDB(t)
	ident := fmt.Sprintf("public.orders:resolved:%d", time.Now().UnixNano())
	cleanupMigrationIdent(t, ident)
	store := NewMigrationSafetyFindingStore(pool)

	firstID := mustUpsertMigration(t, ctx, store, migrationUpsertInput(ident))
	mustUpsertMigration(t, ctx, store, migrationUpsertInput(ident))
	if _, err := pool.Exec(ctx,
		`UPDATE sage.findings SET status = 'resolved', resolved_at = now()
		  WHERE id = $1`, firstID); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	secondID := mustUpsertMigration(t, ctx, store, migrationUpsertInput(ident))
	if secondID == firstID {
		t.Fatalf("upsert after resolve reused resolved id %d", firstID)
	}
	if open := readMigrationRows(t, ctx, ident, "open"); open.rows != 1 ||
		open.occurrences != 1 {
		t.Fatalf("open rows=%d occurrences=%d, want 1 and 1", open.rows, open.occurrences)
	}
	if done := readMigrationRows(t, ctx, ident, "resolved"); done.rows != 1 ||
		done.occurrences != 2 {
		t.Fatalf("resolved rows=%d occurrences=%d, want 1 and 2",
			done.rows, done.occurrences)
	}
}

// Happy path: a repeat detection refreshes the mutable fields (severity,
// title) of the open row rather than keeping the first values.
func TestMigrationSafetyUpsertRefreshesOpenRowFields(t *testing.T) {
	pool, ctx := requireMigrationSafetyDB(t)
	ident := fmt.Sprintf("public.orders:refresh:%d", time.Now().UnixNano())
	cleanupMigrationIdent(t, ident)
	store := NewMigrationSafetyFindingStore(pool)

	mustUpsertMigration(t, ctx, store, migrationUpsertInput(ident))
	next := migrationUpsertInput(ident)
	next.Severity, next.Title = "critical", "Dangerous DDL: escalated"
	mustUpsertMigration(t, ctx, store, next)

	got := readMigrationRows(t, ctx, ident, "open")
	if got.rows != 1 || got.occurrences != 2 || got.severity != "critical" ||
		got.title != "Dangerous DDL: escalated" {
		t.Fatalf("open row = %+v, want 1 row, 2 occurrences, critical, escalated title", got)
	}
}

// Error propagation: a cancelled context surfaces a wrapped error that
// names the operation, and no row is written.
func TestMigrationSafetyUpsertCancelledContextNamesOperation(t *testing.T) {
	pool, ctx := requireMigrationSafetyDB(t)
	ident := fmt.Sprintf("public.orders:cancel:%d", time.Now().UnixNano())
	cleanupMigrationIdent(t, ident)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	id, err := NewMigrationSafetyFindingStore(pool).UpsertMigrationSafetyFinding(
		cancelled, migrationUpsertInput(ident))
	if err == nil || id != 0 {
		t.Fatalf("id=%d err=%v, want 0 and an error", id, err)
	}
	if !strings.Contains(err.Error(), "upsert migration safety finding") {
		t.Fatalf("error %q does not name the upsert", err)
	}
	if got := readMigrationRows(t, ctx, ident, "open"); got.rows != 0 {
		t.Fatalf("cancelled upsert wrote %d rows", got.rows)
	}
}

func mustUpsertMigration(
	t *testing.T, ctx context.Context, store *MigrationSafetyFindingStore,
	input migration.MigrationSafetyFinding,
) int64 {
	t.Helper()
	id, err := store.UpsertMigrationSafetyFinding(ctx, input)
	if err != nil || id <= 0 {
		t.Fatalf("upsert: id=%d err=%v", id, err)
	}
	return id
}
