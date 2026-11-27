package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

const routingTable = "concurrently_log"

func setupRoutingTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS public.`+routingTable+`;
		CREATE TABLE public.`+routingTable+` (id int)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS public.`+routingTable)
	})
}

func routingFinding() analyzer.Finding {
	return analyzer.Finding{
		Category: "table_storage", ObjectType: "table",
		ObjectIdentifier: "public." + routingTable,
		RecommendedSQL:   "ALTER TABLE public." + routingTable + " SET (fillfactor=90)",
	}
}

// executor-08 boundary: a table named concurrently_log is no CONCURRENTLY
// statement. Its ALTER TABLE runs in a transaction and, behind an ACCESS
// EXCLUSIVE holder, gives up at the 1000ms policy ceiling, not at the 30s
// safety.lock_timeout_ms a top-level statement keeps.
func TestConcurrentlyNamedTableRunsInTransactionUnderCeiling(t *testing.T) {
	pool, ctx := requireDB(t)
	setupRoutingTable(t, ctx, pool)
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx,
		`LOCK TABLE public.`+routingTable+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock table: %v", err)
	}
	started := time.Now()
	err = exec.runFindingSQL(ctx, routingFinding(), ActionPolicyDecision{LockCeilingMS: 1000})
	elapsed := time.Since(started)
	if !errors.Is(err, ErrLockNotAvailable) {
		t.Fatalf("runFindingSQL error = %v, want ErrLockNotAvailable", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("lock wait took %s: the statement kept the 30s safety lock timeout "+
			"(routed top-level by its table name), not the 1000ms ceiling", elapsed)
	}
}

// Without a blocker the in-transaction route applies the change.
func TestConcurrentlyNamedTableAlterApplies(t *testing.T) {
	pool, ctx := requireDB(t)
	setupRoutingTable(t, ctx, pool)
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	err := exec.runFindingSQL(ctx, routingFinding(), ActionPolicyDecision{LockCeilingMS: 1000})
	if err != nil {
		t.Fatalf("runFindingSQL: %v", err)
	}
	var opts []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(reloptions, '{}') FROM pg_class
		WHERE oid = 'public.`+routingTable+`'::regclass`).Scan(&opts); err != nil {
		t.Fatalf("read reloptions: %v", err)
	}
	if strings.Join(opts, ",") != "fillfactor=90" {
		t.Fatalf("reloptions = %v, want [fillfactor=90]", opts)
	}
}

// A database-wide VACUUM has no contract and no top-level route: should it
// ever reach execution it fails closed inside a transaction block instead
// of vacuuming every table.
func TestBareVacuumFailsClosedInTransaction(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	finding := analyzer.Finding{ObjectType: "table", RecommendedSQL: "VACUUM"}
	err := exec.runFindingSQL(ctx, finding, ActionPolicyDecision{})
	if err == nil || !strings.Contains(err.Error(), "transaction block") {
		t.Fatalf("bare VACUUM error = %v, want a refusal inside a transaction block", err)
	}
}

func insertRoutingDecision(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.decision
		(feature, intent, target_objects, policy_version, verdict, risk_tier, reason,
		 evidence_id)
		VALUES ('schema_change', 'schema_change', $1, 1, 'execute', 'moderate',
		        'authorized', $2) RETURNING id`,
		`["public.routing_idx"]`,
		fmt.Sprintf("exec08-%s-%d", t.Name(), time.Now().UnixNano())).Scan(&id)
	if err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	return id
}

// Integration: an approved REINDEX INDEX CONCURRENTLY still runs top-level
// (inside a transaction block PostgreSQL refuses it) and holds the typed
// lease on the index it names, by name and by OID, while it runs.
func TestReindexIndexConcurrentlyRunsTopLevelUnderIndexLease(t *testing.T) {
	pool, ctx := requireDB(t)
	setupRoutingTable(t, ctx, pool)
	if _, err := pool.Exec(ctx, `CREATE INDEX routing_idx ON public.`+
		routingTable+` (id)`); err != nil {
		t.Fatalf("create index: %v", err)
	}
	var indexOID uint32
	if err := pool.QueryRow(ctx, `SELECT 'public.routing_idx'::regclass::oid`).
		Scan(&indexOID); err != nil {
		t.Fatalf("index oid: %v", err)
	}
	exec := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	finding := analyzer.Finding{Category: "index_bloat", ObjectType: "index",
		ObjectIdentifier: "public.routing_idx",
		RecommendedSQL:   "REINDEX INDEX CONCURRENTLY public.routing_idx"}
	decision := ActionPolicyDecision{Decision: PolicyDecisionExecute,
		DecisionID: insertRoutingDecision(t, ctx, pool)}
	var leaseKeys []string
	_, err := exec.Apply(ctx, ActionIntent{
		Request: findingRequest(finding, false), Lease: &finding, WaitForSlot: true,
		Authorize: func(context.Context) (ActionPolicyDecision, error) { return decision, nil },
		Execute: func(ctx context.Context, d ActionPolicyDecision) (int64, error) {
			if err := pool.QueryRow(ctx, `SELECT COALESCE(array_agg(object_key ORDER BY
				object_key), '{}') FROM sage.change_lease
				WHERE state='active' AND decision_id=$1`, d.DecisionID).
				Scan(&leaseKeys); err != nil {
				return 0, err
			}
			return 0, exec.runFindingSQL(ctx, finding, d)
		},
	})
	if err != nil {
		t.Fatalf("Apply REINDEX INDEX CONCURRENTLY: %v", err)
	}
	joined := strings.Join(leaseKeys, ",")
	if !strings.Contains(joined, "public.routing_idx") ||
		!strings.Contains(joined, fmt.Sprintf("oid:%d", indexOID)) {
		t.Fatalf("lease keys = %v, want the index by name and oid:%d", leaseKeys, indexOID)
	}
}
