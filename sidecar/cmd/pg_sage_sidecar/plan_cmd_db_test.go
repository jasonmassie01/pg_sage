package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// CHECK-25 (plan-03a): the CLI approves a plan against a local sidecar (an
// httptest server over the real router, real session middleware and the
// real token store) with an operator token, and the approval is the
// token owner's, via cli. A read-only token and an unknown token are
// refused with exit 3 and change nothing (an agent token's refusal is in
// internal/api's plan_routes_test.go).

type cliYes struct{}

func (cliYes) FindingExists(context.Context, int64) (bool, error)           { return true, nil }
func (cliYes) EvidenceExists(context.Context, string, string) (bool, error) { return true, nil }
func (cliYes) TargetCalls(context.Context, []int64, time.Duration) (int64, error) {
	return 5000, nil
}

func cliPlan(t *testing.T, pool *pgxpool.Pool) changeplan.Plan {
	t.Helper()
	p, err := changeplan.NewStore(pool).Create(context.Background(), changeplan.Plan{
		Goal: "cut blocks per call", Origin: changeplan.OriginPerson, CreatedBy: "alice",
		Scope: changeplan.Scope{Objects: []string{"public.orders"}, QueryIDs: []int64{101}},
		Steps: []changeplan.Step{{Ordinal: 1, Kind: changeplan.KindAction,
			ActionType:  "create_index_concurrently",
			ProposedSQL: "CREATE INDEX CONCURRENTLY idx_cli ON public.orders (a)",
			RollbackSQL: "DROP INDEX CONCURRENTLY public.idx_cli", FindingID: ptrInt64(11),
			TargetQueryIDs: []int64{101}, Metric: changeplan.MetricBlocksPerCall,
			ExpectedDirection: changeplan.DirectionDown, ExpectedSource: changeplan.SourceHypoPG,
			DeadlineMinutes: 60}}}, cliYes{})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return p
}

func ptrInt64(v int64) *int64 { return &v }

// cliToken issues an operator token with scopes for a new operator.
func cliToken(t *testing.T, pool *pgxpool.Pool, scopes []string) (string, int) {
	t.Helper()
	ctx := context.Background()
	id, err := auth.CreateUser(ctx, pool, fmt.Sprintf("cli-%d@test.local",
		time.Now().UnixNano()), "correct horse battery", auth.RoleOperator)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	tok, err := mcptoken.NewStore(pool).Create(ctx, mcptoken.CreateRequest{
		Name: fmt.Sprintf("cli-%d", time.Now().UnixNano()), Kind: mcptoken.KindOperator,
		Scopes: scopes, Databases: []string{"*"}, ExpiresIn: time.Hour, OwnerUserID: id,
		CreatedBy: "cli@test.local"})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return tok.Secret, id
}

func cliSidecar(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	c := config.DefaultConfig()
	mgr := fleet.NewManager(c)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: pool,
		Status: &fleet.InstanceStatus{}})
	router, err := api.NewRouterFullRuntime(mgr, c, pool, nil, nil, nil,
		&api.RuntimeDeps{ConfigController: config.NewConfigController(c, nil)},
		api.SessionAuthMiddleware(pool))
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestPlanCLI_ApprovesThroughTheRealRouterWithAnOperatorToken(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "plan_cli"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	p := cliPlan(t, pool)
	url := cliSidecar(t, pool)
	id := strconv.FormatInt(p.ID, 10)
	readOnly, _ := cliToken(t, pool, []string{mcptoken.ScopeRead})
	code, out, errOut := runPlan(t, url, readOnly, "show", id)
	if code != planExitOK || !strings.Contains(out, p.PlanHash) {
		t.Fatalf("show with a read token: exit %d, out %q, err %q", code, out, errOut)
	}
	if code, _, errOut := runPlan(t, url, readOnly, "approve", id, "--hash", p.PlanHash,
		"--all"); code != planExitAuth || !strings.Contains(errOut, "scope_missing") {
		t.Fatalf("approve with a read token: exit %d, err %q, want 3 scope_missing", code,
			errOut)
	}
	if code, _, _ := runPlan(t, url, "pgs_mcp_not_a_real_token", "list"); code != planExitAuth {
		t.Fatalf("an unknown token: exit %d, want 3", code)
	}
	approver, owner := cliToken(t, pool, []string{mcptoken.ScopeRead, mcptoken.ScopeApprove})
	stale := strings.Repeat("0", 64)
	if code, _, errOut := runPlan(t, url, approver, "approve", id, "--hash", stale,
		"--all"); code != planExitRefused || !strings.Contains(errOut, "content_changed") {
		t.Fatalf("a stale hash: exit %d, err %q, want 1 content_changed", code, errOut)
	}
	code, out, errOut = runPlan(t, url, approver, "approve", id, "--hash", p.PlanHash, "--all")
	if code != planExitOK || !strings.Contains(out, "approved (all") {
		t.Fatalf("approve: exit %d, out %q, err %q", code, out, errOut)
	}
	approvals, err := changeplan.NewStore(pool).Approvals(ctx, p.ID)
	if err != nil || len(approvals) != 1 || approvals[0].ApproverUserID != owner ||
		approvals[0].Via != changeplan.ViaCLI || approvals[0].Mode != changeplan.ModeAll {
		t.Fatalf("approvals %+v (%v), want one approve-all via cli by user %d", approvals,
			err, owner)
	}
}
