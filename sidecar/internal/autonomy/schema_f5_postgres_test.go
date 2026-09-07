package autonomy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

func TestRetentionDryRunThenBoundedEnforcementIsDurable(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	table := fmt.Sprintf("f5_retention_%d", time.Now().UnixNano())
	_, err := pool.Exec(ctx, "CREATE TABLE "+table+
		" (id bigint PRIMARY KEY, created_at timestamptz NOT NULL)")
	if err != nil {
		t.Fatalf("create retention fixture: %v", err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO "+table+" VALUES "+
		"(1,now()-interval '60 days'),(2,now()-interval '45 days'),(3,now())")
	if err != nil {
		t.Fatalf("seed retention fixture: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.table_contract
		(schema_name, table_name, append_only, retention_interval,
		 declared_by, evidence_id) VALUES ('public',$1,true,interval '30 days','test',$2)`,
		table, "contract_"+table)
	if err != nil {
		t.Fatalf("declare retention contract: %v", err)
	}
	if _, err := policy.NewStore(pool).Bootstrap(
		ctx, policy.Scope{}, "unattended", "f5-acceptance",
	); err != nil {
		t.Fatalf("bootstrap retention consent: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.table_contract WHERE table_name=$1", table)
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.retention_run WHERE table_name=$1", table)
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})

	guard, err := NewPostgresSchemaGuard(
		pool, "testdb", &recordingRouter{},
		ledger.NewService(ledger.NewPostgresRepository(pool)),
	)
	if err != nil {
		t.Fatalf("NewPostgresSchemaGuard: %v", err)
	}
	if _, err := guard.Scan(ctx); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	if rows := countF5Rows(t, pool, table); rows != 3 {
		t.Fatalf("first retention run deleted rows: %d remain", rows)
	}
	var disposition string
	var candidates, deleted int64
	if err := pool.QueryRow(ctx, `SELECT disposition, candidate_rows, deleted_rows
		FROM sage.retention_run WHERE table_name=$1 ORDER BY id DESC LIMIT 1`, table).
		Scan(&disposition, &candidates, &deleted); err != nil {
		t.Fatalf("read durable dry run: %v", err)
	}
	if disposition != "dry_run" || candidates != 2 || deleted != 0 {
		t.Fatalf("dry run = %s/%d/%d", disposition, candidates, deleted)
	}

	if _, err := guard.Scan(ctx); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if rows := countF5Rows(t, pool, table); rows != 1 {
		t.Fatalf("enforced retention left %d rows, want 1", rows)
	}
	if err := pool.QueryRow(ctx, `SELECT disposition, deleted_rows
		FROM sage.retention_run WHERE table_name=$1 ORDER BY id DESC LIMIT 1`, table).
		Scan(&disposition, &deleted); err != nil {
		t.Fatalf("read durable enforcement: %v", err)
	}
	if disposition != "applied" || deleted != 2 {
		t.Fatalf("enforcement = %s/%d", disposition, deleted)
	}
}

func TestPostgresDetectorFindsEverythingTextAndTypeTightening(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	table := fmt.Sprintf("f5_text_%d", time.Now().UnixNano())
	_, err := pool.Exec(ctx, "CREATE TABLE "+table+
		" (agent_id text PRIMARY KEY, count_text text, payload text)")
	if err != nil {
		t.Fatalf("create everything-text fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})

	items, err := (postgresSchemaDetector{pool}).Detect(ctx)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !hasF5Invariant(items, schemaguard.InvariantEverythingText, table) {
		t.Fatalf("everything-text invariant missing from %#v", items)
	}
	if !hasF5Invariant(items, schemaguard.InvariantTypeTightening, table) {
		t.Fatalf("type-tightening invariant missing from %#v", items)
	}
}

func TestFKRemediationRoutesToVerifiedLifecycleNotGenericDDL(t *testing.T) {
	generic := &recordingRouter{}
	verified := &recordingVerifiedIndexRouter{}
	router := schemaRemediationRouter{
		database: "orders", router: generic, verifiedIndexes: verified,
	}
	item := schemaguard.Remediation{
		Invariant: schemaguard.Invariant{
			Kind:   schemaguard.InvariantMissingFKIndex,
			Schema: "public", Table: "orders",
			ProposedSQL: "CREATE INDEX CONCURRENTLY orders_customer_idx " +
				"ON public.orders(customer_id)",
			RollbackSQL: "DROP INDEX CONCURRENTLY public.orders_customer_idx",
			QueryIDs:    []int64{71, 72},
		},
		Decision: schemaguard.Decision{
			Route:                schemaguard.RouteVerifyIndex,
			Disposition:          schemaguard.DispositionApply,
			RequiresVerification: true,
		},
	}
	if err := router.Route(context.Background(), item); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(generic.routed()) != 0 || verified.calls != 1 {
		t.Fatalf("generic=%#v verified=%d", generic.routed(), verified.calls)
	}
	if !strings.Contains(verified.rollbackSQL, "DROP INDEX CONCURRENTLY") ||
		len(verified.queryIDs) != 2 {
		t.Fatalf("verified route = %#v", verified)
	}
}

type recordingVerifiedIndexRouter struct {
	calls       int
	rollbackSQL string
	queryIDs    []int64
}

func (r *recordingVerifiedIndexRouter) RouteVerifiedIndex(
	_ context.Context, _ Proposal, rollbackSQL string, queryIDs []int64,
) error {
	r.calls++
	r.rollbackSQL = rollbackSQL
	r.queryIDs = append([]int64(nil), queryIDs...)
	return nil
}

func countF5Rows(t *testing.T, pool *pgxpool.Pool, table string) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).
		Scan(&count); err != nil {
		t.Fatalf("count retention rows: %v", err)
	}
	return count
}

func hasF5Invariant(
	items []schemaguard.Invariant, kind schemaguard.InvariantKind, table string,
) bool {
	for _, item := range items {
		if item.Kind == kind && item.Table == table {
			return true
		}
	}
	return false
}
