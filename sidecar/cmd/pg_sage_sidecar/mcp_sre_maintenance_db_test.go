package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// playbooks-06 composed: the MCP statistics maintenance backend on a real
// database. An inconclusive investigation that observed a table is
// refreshed, proposed against the server-resolved newest evidence and
// queued for a person; nothing executes, and rollback refuses a
// non-admin before reaching the service.

type maintenanceMCPFixture struct {
	access *fleetMCPAccess
	inv    sre.Investigation
	table  string
	ctx    context.Context
}

func newMaintenanceMCPFixture(t *testing.T) *maintenanceMCPFixture {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool := openComposedPool(t, ctx, dsn)
	table := createStatsTable(t, ctx, pool)
	name := pool.Config().ConnConfig.Database
	policy := &probes.ReadPolicy{Version: 1, Scope: "database", Database: name,
		Binding: probes.PoolBinding(pool), Allowed: []probes.ID{probes.StatTables}}
	runner := probes.NewRunner(pool, probes.Catalog(), nil, policy)
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	c, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st, Runner: runner,
		ReadPolicy: policy, Config: sre.DefaultCoordinatorConfig("mcp-maint:" + table)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	inv := inconclusiveTableRun(t, ctx, st, c, runner, table)
	svc := sre.NewService(name, c, st)
	cfg := config.DefaultConfig()
	cfg.CloudEnvironment, cfg.Trust.Level, cfg.Safety.QueryTimeoutMs = "lakebase", "advisory", 10000
	e := executor.New(pool, cfg, time.Time{}, func(string, string, ...any) {})
	e.WithDatabaseName(name)
	enableOwnPolicy(t, ctx, pool, e)
	t.Cleanup(func() { e.Shutdown(context.Background()) })
	actions, err := newSREActions(sreActionDeps{readPolicy: policy, service: svc,
		monitored: pool, executor: e, name: name, settings: cfg.SRE.Actions})
	if err != nil || actions.MaintenanceService() == nil {
		t.Fatalf("actions: %v (maintenance attached: %v)", err, actions != nil &&
			actions.MaintenanceService() != nil)
	}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool, Executor: e,
		Status: &fleet.InstanceStatus{}, Investigations: svc, Actions: actions})
	return &maintenanceMCPFixture{access: &fleetMCPAccess{manager: mgr, fallback: pool},
		inv: inv, table: table, ctx: ctx}
}

// enableOwnPolicy gives the executor an "unattended" policy of its own
// scope: other tests in this package bootstrap the unscoped policy as
// "staffed", whose maintenance window would refuse the request step.
func enableOwnPolicy(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	e *executor.Executor) {
	t.Helper()
	policyID := 870000 + int(time.Now().UnixNano()%100000)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.policy WHERE database_id=$1",
			policyID)
	})
	if err := e.EnableStandingPolicy(ctx, "unattended", &policyID); err != nil {
		t.Fatal(err)
	}
}

// createStatsTable is an analyzed table with two correlated columns.
func createStatsTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	table := fmt.Sprintf("mcp_stats_%d", time.Now().UnixNano())
	for _, sql := range []string{"CREATE TABLE public." + table + "(a int, b int)",
		"INSERT INTO public." + table + " SELECT g%10, g%10 FROM generate_series(1,500) g",
		"ANALYZE public." + table} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { pool.Exec(context.Background(), "DROP TABLE public."+table+" CASCADE") })
	return table
}

// inconclusiveTableRun is an investigation with one stat_tables step that
// ended inconclusive.
func inconclusiveTableRun(t *testing.T, ctx context.Context, st *sre.PostgresStore,
	c *sre.Coordinator, runner *probes.Runner, table string) sre.Investigation {
	t.Helper()
	inv, _, err := c.Start(ctx, sre.Trigger{CaseID: "mcp-maint:" + table,
		Kind: sre.TriggerOperator, Subject: table})
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := c.Scope()
	lease, err := st.Claim(ctx, scope, inv.ID, sre.NewUUID())
	if err != nil {
		t.Fatal(err)
	}
	res := runner.Run(ctx, probes.StatTables, probes.Args{})
	if _, err = st.CommitStep(ctx, lease, sre.StepResult{IdempotencyKey: "observe",
		Results: []probes.Result{res}, NextState: sre.StateEvaluating}); err != nil {
		t.Fatal(err)
	}
	inv, err = st.Conclude(ctx, lease, sre.Conclusion{State: sre.StateInconclusive,
		Summary: sre.Summary{Family: "stat_views"}})
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func (f *maintenanceMCPFixture) as(role string, scopes ...mcp.Scope) context.Context {
	return mcp.WithPrincipal(f.ctx, mcp.Principal{Actor: "token:maint-" + role, Role: role,
		Kind: mcp.KindHuman, Scopes: scopes, TokenID: "maint-" + role, OwnerUserID: 41})
}

func TestMCPStatisticsMaintenanceRefreshProposeRequest(t *testing.T) {
	f := newMaintenanceMCPFixture(t)
	operator := f.as("operator", mcp.ScopeRead, mcp.ScopePropose)
	req := mcp.SREMaintenanceRequest{InvestigationID: string(f.inv.ID), Schema: "public",
		Table: f.table}
	out, err := f.access.RefreshTableEvidence(operator, req)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	refreshed, ok := out.(refreshedEvidence)
	if !ok || refreshed.EvidenceID == "" || len(refreshed.SHA256) != 64 ||
		refreshed.FreshUntil.Sub(refreshed.ObservedAt) != 15*time.Minute {
		t.Fatalf("refresh result = %#v", out)
	}
	req.Columns, req.Rationale = []string{"a", "b"}, "operator hypothesis: a and b correlate"
	out, err = f.access.ProposeStatistics(operator, req)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	p := out.(sreaction.MaintenanceProposal)
	if p.Request.EvidenceID != refreshed.EvidenceID || p.State != "proposed" ||
		p.RequestedBy != "mcp:token:maint-operator" {
		t.Fatalf("proposal cites %s as %q (%s), want the refresh %s", p.Request.EvidenceID,
			p.RequestedBy, p.State, refreshed.EvidenceID)
	}
	out, err = f.access.RequestStatistics(operator, mcp.SREMaintenanceRequest{
		ProposalID: string(p.ID)})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if q := out.(sreaction.MaintenanceProposal); q.State != "requested" || q.QueueID == 0 {
		t.Fatalf("request = %+v, want one queued approval item", q)
	}
	f.checkRollbackRefusals(t, p.ID)
	if _, err = f.access.RefreshTableEvidence(operator, mcp.SREMaintenanceRequest{
		Database: "no_such_db", InvestigationID: string(f.inv.ID), Schema: "public",
		Table: f.table}); !errors.Is(err, sre.ErrNotFound) {
		t.Fatalf("unknown database = %v, want ErrNotFound", err)
	}
	if _, err = f.access.RequestStatistics(operator, mcp.SREMaintenanceRequest{
		ProposalID: "not-a-uuid"}); !errors.Is(err, sre.ErrInvalidRequest) {
		t.Fatalf("malformed proposal id = %v, want ErrInvalidRequest", err)
	}
}

// checkRollbackRefusals: an operator is refused before the service; an
// admin reaches it, which refuses a proposal that never executed.
func (f *maintenanceMCPFixture) checkRollbackRefusals(t *testing.T, id sre.UUID) {
	t.Helper()
	rollback := mcp.SREMaintenanceRequest{ProposalID: string(id), Reason: "undo"}
	operator := f.as("operator", mcp.ScopeRead, mcp.ScopePropose, mcp.ScopeApprove)
	if _, err := f.access.RollbackStatistics(operator, rollback); !errors.Is(err,
		mcp.ErrAdminRequired) {
		t.Fatalf("operator rollback = %v, want ErrAdminRequired", err)
	}
	admin := f.as("admin", mcp.ScopeRead, mcp.ScopePropose, mcp.ScopeApprove)
	if _, err := f.access.RollbackStatistics(admin, rollback); !errors.Is(err,
		sreaction.ErrProposalState) {
		t.Fatalf("admin rollback of an unexecuted proposal = %v, want ErrProposalState", err)
	}
}
