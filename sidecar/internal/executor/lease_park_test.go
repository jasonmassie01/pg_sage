package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// D1 serialize_mode=park: a DDL lease conflict parks the action. It writes
// no failed action_log row, so it neither counts toward retries/abandonment
// nor consumes the self-initiated rate budget, and it is retried next cycle.

const parkTable = "d1_park_target"

func insertParkDecision(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.decision
		(feature, intent, target_objects, policy_version, verdict, risk_tier, reason,
		 evidence_id)
		VALUES ('schema_change', 'schema_change', $1, 1, 'execute', 'moderate',
		        'authorized', $2) RETURNING id`,
		`["public.`+parkTable+`"]`,
		fmt.Sprintf("d1-park-%s-%d", t.Name(), time.Now().UnixNano())).Scan(&id)
	if err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	return id
}

func setupParkTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS public.`+parkTable+`;
		CREATE TABLE public.`+parkTable+` (id int)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS public.`+parkTable)
		closeTestMonitors(t, pool, parkFinding().RecommendedSQL)
	})
}

func parkFinding() analyzer.Finding {
	return analyzer.Finding{
		Category: "table_storage", ObjectIdentifier: "public." + parkTable,
		Title:          "d1 park fillfactor",
		RecommendedSQL: "ALTER TABLE public." + parkTable + " SET (fillfactor = 90)",
	}
}

func waitForActiveLease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, decision int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var active bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sage.change_lease
			WHERE decision_id = $1 AND state = 'active')`, decision).Scan(&active); err != nil {
			t.Fatalf("read lease: %v", err)
		}
		if active {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("first executor never took its DDL lease")
}

func actionRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, decision int64) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT outcome FROM sage.action_log WHERE decision_id = $1`, decision)
	if err != nil {
		t.Fatalf("read action_log: %v", err)
	}
	defer rows.Close()
	var outcomes []string
	for rows.Next() {
		var outcome string
		if err := rows.Scan(&outcome); err != nil {
			t.Fatalf("scan: %v", err)
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

func parkDecisions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, parked int64) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.decision
		WHERE verdict = 'parked' AND reason = 'ddl_conflict'
		  AND (evidence->>'parked_decision_id')::bigint = $1`, parked).Scan(&count); err != nil {
		t.Fatalf("read park decisions: %v", err)
	}
	return count
}

// Two executors race for the same table: the first holds the lease while
// its ALTER waits on a table lock; the second must park exactly once and
// log no action row. The first then completes without a failure row.
func TestLeaseConflictParksWithoutFailureRow(t *testing.T) {
	pool, ctx := requireDB(t)
	setupParkTable(t, ctx, pool)
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	first, second := insertParkDecision(t, ctx, pool), insertParkDecision(t, ctx, pool)

	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := blocker.Exec(ctx,
		`LOCK TABLE public.`+parkTable+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock table: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		exec.executeFinding(ctx, parkFinding(), 0, ActionPolicyDecision{DecisionID: first})
	}()
	waitForActiveLease(t, ctx, pool, first)

	exec.executeFinding(ctx, parkFinding(), 0, ActionPolicyDecision{DecisionID: second})

	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release blocker: %v", err)
	}
	wg.Wait()

	if rows := actionRows(t, ctx, pool, second); len(rows) != 0 {
		t.Fatalf("parked action wrote action_log rows %v, want none", rows)
	}
	if got := parkDecisions(t, ctx, pool, second); got != 1 {
		t.Fatalf("park/ddl_conflict decisions for %d = %d, want exactly 1", second, got)
	}
	if got := parkDecisions(t, ctx, pool, first); got != 0 {
		t.Fatalf("winning decision %d was parked %d times, want 0", first, got)
	}
	rows := actionRows(t, ctx, pool, first)
	if len(rows) != 1 || rows[0] == "failed" {
		t.Fatalf("winning action rows = %v, want one non-failed row", rows)
	}
}

// A park does not count toward exceedsMaxRetries: repeated conflicts for a
// finding never abandon it.
func TestLeaseConflictDoesNotCountTowardRetries(t *testing.T) {
	pool, ctx := requireDB(t)
	setupParkTable(t, ctx, pool)
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	var findingID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, recommendation)
		VALUES ('table_storage', 'info', 'table', $1, 'd1 park', '{}', 'r')
		RETURNING id`, "public."+parkTable).Scan(&findingID); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.findings WHERE id = $1`, findingID)
	})
	holder := insertParkDecision(t, ctx, pool)
	objects, _ := policy.NormalizeTargetObjects([]string{"public." + parkTable})
	manager := policy.NewPostgresLeaseManager(pool, nil, holder, time.Minute)
	leaseID, err := manager.AcquireLease(ctx, "other-writer", objects, "hold")
	if err != nil {
		t.Fatalf("hold lease: %v", err)
	}
	defer func() { _ = manager.ReleaseLease(ctx, leaseID) }()

	for attempt := 0; attempt < maxActionRetries+1; attempt++ {
		decision := insertParkDecision(t, ctx, pool)
		exec.executeFinding(ctx, parkFinding(), findingID,
			ActionPolicyDecision{DecisionID: decision})
		if got := parkDecisions(t, ctx, pool, decision); got != 1 {
			t.Fatalf("attempt %d: park decisions = %d, want 1", attempt, got)
		}
	}
	if exec.exceedsMaxRetries(ctx, findingID) {
		t.Fatal("lease conflicts counted toward max retries; finding abandoned")
	}
}

func TestParkLeaseConflictIgnoresOtherErrors(t *testing.T) {
	exec := New(nil, config.DefaultConfig(), time.Time{}, nopLog)
	if exec.parkLeaseConflict(context.Background(), parkFinding(), 1,
		errors.New("normalize DDL lease targets: bad")) {
		t.Fatal("a non-conflict lease error was treated as a park")
	}
	if !exec.parkLeaseConflict(context.Background(), parkFinding(), 1,
		fmt.Errorf("wrapped: %w", policy.ErrLeaseConflict)) {
		t.Fatal("a wrapped lease conflict was not treated as a park")
	}
}
