package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// plan-03a on real tables: the plan routes through the full router read
// and decide plans stored by changeplan, in standalone mode (one database)
// and in fleet mode (two databases whose plan ids collide).

// yesResolver answers every Validate lookup as present and busy.
type yesResolver struct{}

func (yesResolver) FindingExists(context.Context, int64) (bool, error) { return true, nil }

func (yesResolver) EvidenceExists(context.Context, string, string) (bool, error) {
	return true, nil
}

func (yesResolver) TargetCalls(context.Context, []int64, time.Duration) (int64, error) {
	return 5000, nil
}

func (yesResolver) ManagedChangeExists(context.Context, int64) (bool, error) { return true, nil }
func (yesResolver) SettingExists(context.Context, string) (bool, error)      { return true, nil }

// planDB is a fresh bootstrapped database.
func planDB(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

// twoStepPlan stores a plan of two index steps (step 2 after step 1).
func twoStepPlan(t *testing.T, pool *pgxpool.Pool, goal string) changeplan.Plan {
	t.Helper()
	step := func(ordinal int, name string) changeplan.Step {
		return changeplan.Step{Ordinal: ordinal, Kind: changeplan.KindAction,
			ActionType:  "create_index_concurrently",
			ProposedSQL: "CREATE INDEX CONCURRENTLY " + name + " ON public.orders (a)",
			RollbackSQL: "DROP INDEX CONCURRENTLY public." + name,
			FindingID:   ptrTo(int64(11)), TargetQueryIDs: []int64{101},
			Metric:            changeplan.MetricBlocksPerCall,
			ExpectedDirection: changeplan.DirectionDown, ExpectedPct: ptrTo(30.0),
			ExpectedSource: changeplan.SourceHypoPG, DeadlineMinutes: 60,
			MinEvidence: changeplan.MinEvidence{MinCalls: 100, MinSamples: 30}}
	}
	p, err := changeplan.NewStore(pool).Create(context.Background(), changeplan.Plan{
		Goal: goal, Scope: changeplan.Scope{Objects: []string{"public.orders"},
			QueryIDs: []int64{101}}, Origin: changeplan.OriginPerson, CreatedBy: "alice",
		Steps: []changeplan.Step{step(1, "idx_api_a"), step(2, "idx_api_b")},
		Edges: []changeplan.Edge{{From: 1, To: 2}}}, yesResolver{})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return p
}

func planRouter(t *testing.T, mgr *fleet.DatabaseManager, user *auth.User) http.Handler {
	t.Helper()
	return actionRouter(t, mgr, user)
}

func planStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM sage.plan
		WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestPlanRoutesDB_StandaloneApproveUpToThenTheStep(t *testing.T) {
	pool := planDB(t, "plan_api_one")
	p := twoStepPlan(t, pool, "speed up the order lookups")
	mgr := fleet.NewManager(config.DefaultConfig())
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: pool,
		Status: &fleet.InstanceStatus{}})
	op := planRouter(t, mgr, testOperatorUser())
	code, out, body := planCall(t, op, http.MethodGet, "/api/v1/plans", "", "")
	if plans, _ := out["plans"].([]any); code != 200 || len(plans) != 1 {
		t.Fatalf("list: %d %s", code, body)
	}
	base := fmt.Sprintf("/api/v1/plans/%d", p.ID)
	stale := `{"plan_hash":"` + goldenHash + `","mode":"up_to","up_to":1}`
	if code, out, _ := planCall(t, op, http.MethodPost, base+"/approve", stale,
		""); code != http.StatusConflict || out["code"] != "content_changed" {
		t.Fatalf("stale hash: %d %v, want 409 content_changed", code, out)
	}
	past := `{"plan_hash":"` + p.PlanHash + `","mode":"up_to","up_to":3}`
	if code, out, _ := planCall(t, op, http.MethodPost, base+"/approve", past,
		""); code != 400 || out["code"] != changeplan.CodeInvalidUpTo {
		t.Fatalf("up_to past the steps: %d %v, want 400 invalid_up_to", code, out)
	}
	upTo := `{"plan_hash":"` + p.PlanHash + `","mode":"up_to","up_to":1}`
	if code, out, body := planCall(t, op, http.MethodPost, base+"/approve", upTo,
		""); code != 200 || out["approval"].(map[string]any)["mode"] != "up_to" {
		t.Fatalf("approve up to 1: %d %s", code, body)
	}
	cov := planCoverage(t, op, base)
	if cov[0]["covered"] != true || cov[1]["covered"] != false {
		t.Fatalf("coverage %v, want step 1 only", cov)
	}
	step2 := fmt.Sprintf("%s/steps/%d/approve", base, p.Steps[1].ID)
	if code, _, body := planCall(t, op, http.MethodPost, step2,
		`{"plan_hash":"`+p.PlanHash+`"}`, ""); code != 200 {
		t.Fatalf("approve step 2: %d %s", code, body)
	}
	if cov := planCoverage(t, op, base); cov[1]["covered"] != true || cov[1]["by"] != "step" {
		t.Fatalf("coverage %v, want step 2 covered by its own approval", cov)
	}
	if code, out, _ := planCall(t, op, http.MethodPost, base+"/reject", `{"reason":"no"}`,
		""); code != http.StatusConflict || out["code"] != "not_pending" {
		t.Fatalf("reject an approved plan: %d %v, want 409 not_pending", code, out)
	}
	if code, _, body := planCall(t, op, http.MethodPost, base+"/cancel", "", ""); code != 200 {
		t.Fatalf("cancel: %d %s", code, body)
	}
	if st := planStatus(t, pool, p.ID); st != "cancelled" {
		t.Fatalf("stored status %s, want cancelled", st)
	}
}

func planCoverage(t *testing.T, h http.Handler, base string) []map[string]any {
	t.Helper()
	code, out, body := planCall(t, h, http.MethodGet, base, "", "")
	if code != 200 {
		t.Fatalf("detail: %d %s", code, body)
	}
	var cov []map[string]any
	for _, c := range out["coverage"].([]any) {
		cov = append(cov, c.(map[string]any))
	}
	if len(cov) != 2 {
		t.Fatalf("coverage %v, want two steps", cov)
	}
	return cov
}

// Fleet mode: both databases hold plan 1; a decision on one database
// never touches the other's.
func TestPlanRoutesDB_FleetDecidesOnTheNamedDatabaseOnly(t *testing.T) {
	a, b := planDB(t, "plan_api_a"), planDB(t, "plan_api_b")
	pa, pb := twoStepPlan(t, a, "orders plan"), twoStepPlan(t, b, "billing plan")
	if pa.ID != pb.ID {
		t.Fatalf("plan ids %d and %d; the test needs them equal", pa.ID, pb.ID)
	}
	mgr := fleet.NewManager(config.DefaultConfig())
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: a,
		Status: &fleet.InstanceStatus{}})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "billing", Pool: b,
		Status: &fleet.InstanceStatus{}})
	op := planRouter(t, mgr, testOperatorUser())
	code, out, _ := planCall(t, op, http.MethodGet, "/api/v1/plans", "", "")
	if plans, _ := out["plans"].([]any); code != 200 || len(plans) != 2 {
		t.Fatalf("fleet list: %d %v, want both plans", code, out)
	}
	path := fmt.Sprintf("/api/v1/plans/%d/approve?database=billing", pb.ID)
	body := `{"plan_hash":"` + pb.PlanHash + `","mode":"all"}`
	if code, out, raw := planCall(t, op, http.MethodPost, path, body, ""); code != 200 ||
		out["database"] != "billing" {
		t.Fatalf("approve on billing: %d %s", code, raw)
	}
	if sa, sb := planStatus(t, a, pa.ID), planStatus(t, b, pb.ID); sa != "proposed" ||
		sb != "approved" {
		t.Fatalf("statuses orders %s billing %s, want proposed and approved", sa, sb)
	}
	wrong := fmt.Sprintf("/api/v1/plans/%d/approve?database=orders", pa.ID)
	if code, out, _ := planCall(t, op, http.MethodPost, wrong, body, ""); code != 409 ||
		out["code"] != "content_changed" {
		t.Fatalf("billing's hash on orders: %d %v, want 409 content_changed", code, out)
	}
	viewer := planRouter(t, mgr, testViewerUser())
	if code, _, _ := planCall(t, viewer, http.MethodPost, path, body, ""); code != 403 {
		t.Fatalf("viewer approves: %d, want 403", code)
	}
}
