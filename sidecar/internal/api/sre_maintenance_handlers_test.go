package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func maintenanceAPIFixture(t *testing.T) (*fleet.DatabaseManager, *sre.Service, sreaction.StatisticsRequest) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "maintenance_api"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"CREATE TABLE public.fixture(a int,b int)", "INSERT INTO public.fixture SELECT g%10,g%10 FROM generate_series(1,1000)g", "ANALYZE public.fixture"} {
		if _, err = pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	rp := &probes.ReadPolicy{Version: 1, Scope: "database", Database: pool.Config().ConnConfig.Database, Binding: probes.PoolBinding(pool), Allowed: []probes.ID{probes.StatTables}}
	runner := probes.NewRunner(pool, probes.Catalog(), nil, rp)
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st, Runner: runner, ReadPolicy: rp, Config: sre.DefaultCoordinatorConfig("maintenance-api:" + string(sre.NewUUID()))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = coord.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	inv, _, err := coord.Start(ctx, sre.Trigger{Kind: sre.TriggerOperator, CaseID: "owned-statistics", Subject: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := coord.Scope()
	lease, err := st.Claim(ctx, scope, inv.ID, sre.NewUUID())
	if err != nil {
		t.Fatal(err)
	}
	result := runner.Run(ctx, probes.StatTables, probes.Args{})
	if result.Status != probes.StatusOK {
		t.Fatalf("real table observation=%+v", result)
	}
	if _, err = st.CommitStep(ctx, lease, sre.StepResult{IdempotencyKey: "observed-table", Results: []probes.Result{result}, NextState: sre.StateEvaluating}); err != nil {
		t.Fatal(err)
	}
	ev, err := st.Evidence(ctx, scope, inv.ID)
	if err != nil || len(ev) != 1 {
		t.Fatalf("evidence=%+v err=%v", ev, err)
	}
	inv, err = st.Conclude(ctx, lease, sre.Conclusion{State: sre.StateConcluded,
		Summary: sre.Summary{Family: "stat_views", Root: "table_observed", Conclusive: true},
		Hypotheses: []sre.HypothesisRecord{{GraphVersion: "maintenance-api-fixture-v1", Family: "stat_views",
			Node: "table_observed", Label: "Owned table observed for an operator statistics experiment",
			RefutationProbe: "stat_tables", Status: sre.HypothesisRoot,
			Support: []sre.Fact{{EvidenceID: ev[0].ID, Text: "Owned fixture table counters observed; column correlation is an operator hypothesis"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	svc := sre.NewService("owned", coord, st)
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	cfg.CloudEnvironment = "lakebase"
	cfg.Safety.QueryTimeoutMs = 10000
	exec := executor.New(pool, cfg, time.Time{}, func(string, string, ...any) {})
	exec.WithDatabaseName("owned")
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	t.Cleanup(func() { exec.Shutdown(context.Background()) })
	actions, err := sreaction.NewActionService(sreaction.ActionDeps{Service: svc, Targets: probes.NewRunner(pool, probes.ActionRegistry(), nil, rp), Queue: sreaction.NewPGApprovalQueue(pool, nil), Executor: exec, Config: sreaction.DefaultActionConfig()})
	if err != nil {
		t.Fatal(err)
	}
	maintenance, err := sreaction.NewMaintenanceService(sreaction.MaintenanceDeps{Service: svc, Pool: pool, Executor: exec})
	if err != nil {
		t.Fatal(err)
	}
	actions.SetMaintenanceService(maintenance)
	exec.SetApprovedActionRunner(actions)
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "owned", Pool: pool, Status: &fleet.InstanceStatus{}, Executor: exec, Investigations: svc, Actions: actions})
	request := sreaction.StatisticsRequest{Schema: "public", Table: "fixture", Columns: []string{"a", "b"}, Kinds: []string{"dependencies", "ndistinct"}, EvidenceID: ev[0].ID, EvidenceSHA256: hex.EncodeToString(ev[0].SHA256), Rationale: "Operator hypothesis: owned fixture columns have correlated distributions"}
	return mgr, svc, request
}

func maintenanceAPICall(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSREMaintenanceAPIApprovalAndRollbackLifecycle(t *testing.T) {
	mgr, svc, req := maintenanceAPIFixture(t)
	ctx := context.Background()
	page, err := svc.List(ctx, sre.ListFilter{Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("cases=%+v err=%v", page, err)
	}
	base := "/api/v1/databases/owned/investigations/" + string(page.Items[0].ID) + "/maintenance"
	user := testOperatorUser()
	h := actionRouter(t, mgr, user)
	raw, _ := json.Marshal(req)
	w := maintenanceAPICall(h, "POST", base, string(raw))
	if w.Code != 200 {
		t.Fatalf("propose=%d %s", w.Code, w.Body.String())
	}
	var proposal sreaction.MaintenanceProposal
	if err = json.Unmarshal(w.Body.Bytes(), &proposal); err != nil {
		t.Fatal(err)
	}
	w = maintenanceAPICall(h, "POST", base, string(raw))
	var repeated sreaction.MaintenanceProposal
	_ = json.Unmarshal(w.Body.Bytes(), &repeated)
	if w.Code != 200 || repeated.ID != proposal.ID {
		t.Fatalf("idempotent propose=%d %s", w.Code, w.Body.String())
	}
	mutation := base + "/" + string(proposal.ID)
	w = maintenanceAPICall(h, "POST", mutation+"/request", `{}`)
	if w.Code != 200 {
		t.Fatalf("request=%d %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &proposal)
	var exists bool
	pool := mgr.GetInstance("owned").Pool
	object := "sage_stx_" + strings.ReplaceAll(string(proposal.ID), "-", "")
	if err = pool.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_statistic_ext WHERE stxname=$1)", object).Scan(&exists); err != nil || exists {
		t.Fatalf("request executed SQL: %v %v", exists, err)
	}
	code, body := actionCall(t, h, "POST", fmt.Sprintf("/api/v1/actions/%d/approve?database=owned", proposal.QueueID))
	if code != 200 || body["executed"] != true || body["verification_status"] != "verified" {
		t.Fatalf("approve=%d %+v", code, body)
	}
	if code, _ = actionCall(t, h, "POST", fmt.Sprintf("/api/v1/actions/%d/approve?database=owned", proposal.QueueID)); code == 200 {
		t.Fatal("approval reused")
	}
	w = maintenanceAPICall(h, "POST", mutation+"/rollback", `{"reason":"fixture cleanup"}`)
	if w.Code != 403 {
		t.Fatalf("operator rollback=%d", w.Code)
	}
	admin := *user
	admin.Role = auth.RoleAdmin
	h = actionRouter(t, mgr, &admin)
	w = maintenanceAPICall(h, "POST", mutation+"/rollback", `{"reason":"fixture cleanup"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"rolled_back"`) {
		t.Fatalf("rollback=%d %s", w.Code, w.Body.String())
	}
	if err = pool.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_statistic_ext WHERE stxname=$1)", object).Scan(&exists); err != nil || exists {
		t.Fatalf("rollback left object: %v %v", exists, err)
	}
	scope, err := svc.Scope(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store().VerifyEvents(ctx, scope, page.Items[0].ID); err != nil {
		t.Fatal(err)
	}
	viewer := *user
	viewer.Role = auth.RoleViewer
	h = actionRouter(t, mgr, &viewer)
	for _, path := range []string{base, mutation + "/request", mutation + "/rollback"} {
		if w = maintenanceAPICall(h, "POST", path, `{}`); w.Code != 403 {
			t.Fatalf("viewer mutation=%d", w.Code)
		}
	}
	if w = maintenanceAPICall(h, "GET", base, ""); w.Code != 200 {
		t.Fatalf("viewer read=%d", w.Code)
	}
}
