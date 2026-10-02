package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// The fleet canary reads each database's own action log, verification
// and recommendations; the MCP autonomy tools resolve through the same
// registry as the API.

func TestFleetCanaryTargetReadsTheDatabase(t *testing.T) {
	pool := autonomyPool(t)
	ctx := context.Background()
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommended_sql, status)
		VALUES ('missing_index', 'warning', 'table', 'public.canary_t', 'idx', '{}',
		        'CREATE INDEX CONCURRENTLY  canary_idx ON public.canary_t (a);', 'open')
		RETURNING id`).Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	var actionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		rollback_sql, outcome) VALUES ('create_index_concurrently',
		'CREATE INDEX CONCURRENTLY canary_idx ON public.canary_t (a)',
		'DROP INDEX CONCURRENTLY public.canary_idx', 'success') RETURNING id`).
		Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	target := fleetCanaryTarget{inst: &fleet.DatabaseInstance{Name: "orders", Pool: pool}}
	src, err := target.SourceAction(ctx, actionID)
	if err != nil || src.Outcome != "success" || src.Verification != "" ||
		src.RollbackSQL != "DROP INDEX CONCURRENTLY public.canary_idx" ||
		src.ActionType != "create_index_concurrently" {
		t.Fatalf("source = %+v (%v)", src, err)
	}
	id, found, err := target.MatchingFinding(ctx,
		"create index concurrently canary_idx on public.canary_t (a)")
	if err != nil || !found || id != findingID {
		t.Fatalf("matching finding = %d %v (%v), want %d", id, found, err, findingID)
	}
	if _, found, err := target.MatchingFinding(ctx,
		"CREATE INDEX CONCURRENTLY other ON public.canary_t (b)"); err != nil || found {
		t.Fatalf("a different change matched: %v %v", found, err)
	}
	outcome, verdict, err := target.ActionResult(ctx, actionID)
	if err != nil || outcome != "success" || verdict != "" {
		t.Fatalf("action result = %q %q (%v)", outcome, verdict, err)
	}
	if _, err := target.SourceAction(ctx, -1); err == nil {
		t.Fatal("a missing action was found")
	}
	if _, err := target.Execute(ctx, findingID, "x", "", nil); err == nil {
		t.Fatal("a database without an executor executed")
	}
}

func TestAutonomyMCPBackendResolvesThroughTheRegistry(t *testing.T) {
	pool := autonomyPool(t)
	ledgers := newAutonomyLedgers(true)
	svc, err := ledgers.ledgerFor(context.Background(), pool,
		config.DefaultConfig().SRE.Autonomy)
	if err != nil {
		t.Fatal(err)
	}
	ledgers.registry.Register("orders", earned.RegistryEntry{Service: svc,
		Limiter: svc.Limiter(earned.Binding{Database: "orders"})})
	b := autonomyMCPBackend{registry: ledgers.registry}
	got, err := b.GetAutonomy(context.Background(), mcp.AutonomyRequest{})
	view, ok := got.(map[string]any)
	if err != nil || !ok || view["database"] != "orders" || view["enforced"] != true {
		t.Fatalf("get = %v (%v)", got, err)
	}
	if _, err := b.GetAutonomy(context.Background(),
		mcp.AutonomyRequest{Database: "nope"}); !errors.Is(err, earned.ErrInvalidRequest) {
		t.Fatalf("unknown database: %v", err)
	}
	_, err = b.DowngradeAutonomy(context.Background(), mcp.AutonomyRequest{
		Family: "wal_retention", ActionClass: "wal_bound", Level: "L0",
		Reason: "agent saw replica lag"}, "mcp:user:2")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := svc.Granted(context.Background(), earned.FamilyWAL, earned.ClassWALBound)
	if st.Level != earned.L0 || st.ChangedBy != "mcp:user:2" {
		t.Fatalf("after MCP downgrade = %+v", st)
	}
	if _, err := b.DowngradeAutonomy(context.Background(), mcp.AutonomyRequest{
		Family: "wal_retention", ActionClass: "wal_bound", Level: "L9", Reason: "r"},
		"mcp:user:2"); !errors.Is(err, earned.ErrInvalidRequest) {
		t.Fatalf("bad level: %v", err)
	}
}
