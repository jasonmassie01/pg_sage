package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// fakePlans is one database's plan store for the handler tests: it
// returns detail and records every decision; err fails every call.
type fakePlans struct {
	mu        sync.Mutex
	detail    changeplan.Detail
	err       error
	decisions []string
	users     []int
	decided   changeplan.Decision
	stepVia   changeplan.Via
}

func (f *fakePlans) record(what string, userID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = append(f.decisions, what)
	f.users = append(f.users, userID)
	return f.err
}

func (f *fakePlans) List(context.Context, changeplan.ListFilter) ([]changeplan.Plan, error) {
	if f.err != nil {
		return nil, f.err
	}
	p := f.detail.Plan
	p.Steps, p.Edges = nil, nil
	return []changeplan.Plan{p}, nil
}

func (f *fakePlans) Detail(_ context.Context, id int64) (changeplan.Detail, error) {
	if f.err != nil {
		return changeplan.Detail{}, f.err
	}
	if id != f.detail.Plan.ID {
		return changeplan.Detail{}, changeplan.ErrNotFound
	}
	return f.detail, nil
}

func (f *fakePlans) ApproveWith(_ context.Context, id int64, d changeplan.Decision,
	userID int) (changeplan.Approval, error) {
	f.mu.Lock()
	f.decided = d
	f.mu.Unlock()
	if err := f.record("approve", userID); err != nil {
		return changeplan.Approval{}, err
	}
	return changeplan.Approval{ID: 41, PlanID: id, PlanHash: d.PlanHash,
		ApproverUserID: userID, Mode: d.Mode, Via: d.Via, DecidedAt: goldenTime}, nil
}

func (f *fakePlans) ApproveStep(_ context.Context, id, stepID int64, hash string, userID int,
	via changeplan.Via) (changeplan.StepApproval, error) {
	f.mu.Lock()
	f.stepVia = via
	f.mu.Unlock()
	if err := f.record("approve_step", userID); err != nil {
		return changeplan.StepApproval{}, err
	}
	return changeplan.StepApproval{ID: 51, PlanID: id, StepID: stepID, Ordinal: 1,
		PlanHash: hash, ApproverUserID: userID, Via: via, DecidedAt: goldenTime}, nil
}

func (f *fakePlans) Reject(_ context.Context, _ int64, _ string, userID int) error {
	return f.record("reject", userID)
}

func (f *fakePlans) Resume(_ context.Context, _ int64, _ string, userID int) error {
	return f.record("resume", userID)
}

func (f *fakePlans) Cancel(_ context.Context, _ int64, _ string, userID int) error {
	return f.record("cancel", userID)
}

var goldenTime = time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)

const goldenHash = "4f1c2b0d9e8a7f6e5d4c3b2a19087f6e5d4c3b2a19087f6e5d4c3b2a19087f6e"

// goldenDetail is a three-step plan approved up to step 2, with step 3's
// coverage refused: the shape ux-03's mocks copy (e2e/mocks/plans.ts).
func goldenDetail() changeplan.Detail {
	step := func(id int64, ordinal int, name string) changeplan.Step {
		return changeplan.Step{ID: id, Ordinal: ordinal, Kind: changeplan.KindAction,
			ActionType: "create_index_concurrently",
			ProposedSQL: "CREATE INDEX CONCURRENTLY " + name +
				" ON public.orders (customer_id)",
			RollbackSQL: "DROP INDEX CONCURRENTLY public." + name, FindingID: ptrTo(int64(11)),
			TargetQueryIDs: []int64{101}, Metric: changeplan.MetricBlocksPerCall,
			ExpectedDirection: changeplan.DirectionDown, ExpectedSource: changeplan.SourceHypoPG,
			DeadlineMinutes: 60, OnImproved: changeplan.ActionKeep,
			OnNoChange: changeplan.ActionKeep, OnRegressed: changeplan.ActionRevertStep,
			OnInconclusive: changeplan.ActionHaltAsk, OnExpired: changeplan.ActionHaltAsk,
			Runtime: changeplan.StepRuntime{State: changeplan.StatePending}}
	}
	upTo := 2
	p := changeplan.Plan{ID: 7, Goal: "cut blocks per call on the order lookups",
		Scope:  changeplan.Scope{Objects: []string{"public.orders"}, QueryIDs: []int64{101}},
		Origin: changeplan.OriginPerson, Status: changeplan.StatusApproved, PlanHash: goldenHash,
		CreatedAt: goldenTime, UpdatedAt: goldenTime, CreatedBy: "alice",
		Steps: []changeplan.Step{step(70, 1, "idx_a"), step(71, 2, "idx_b"),
			step(72, 3, "idx_c")},
		Edges: []changeplan.Edge{{From: 1, To: 2}, {From: 2, To: 3}}}
	return changeplan.Detail{Plan: p,
		Approvals: []changeplan.Approval{{ID: 41, PlanID: 7, PlanHash: goldenHash,
			ApproverUserID: 2, Mode: changeplan.ModeUpTo, UpTo: &upTo, Via: changeplan.ViaUI,
			DecidedAt: goldenTime}},
		StepApprovals: []changeplan.StepApproval{},
		Coverage: []changeplan.StepCoverage{
			{StepID: 70, Ordinal: 1, Covered: true, By: changeplan.CoveredByPlan},
			{StepID: 71, Ordinal: 2, Covered: true, By: changeplan.CoveredByPlan},
			{StepID: 72, Ordinal: 3, Reason: "the plan was approved up to step 2"}}}
}

func ptrTo[T any](v T) *T { return &v }

// planFixture is a fleet of the given databases, each served by fake.
type planFixture struct {
	mgr    *fleet.DatabaseManager
	fake   *fakePlans
	tokens *fakeLintTokens
}

func newPlanFixture(t *testing.T, databases ...string) *planFixture {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	mgr := fleet.NewManager(config.DefaultConfig())
	for _, name := range databases {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
			Status: &fleet.InstanceStatus{}})
	}
	return &planFixture{mgr: mgr, fake: &fakePlans{detail: goldenDetail()},
		tokens: lintTokens(map[string]mcptoken.Grant{})}
}

// router serves the plan routes with user injected as the session user.
func (f *planFixture) router(user *auth.User, appMode bool) http.Handler {
	mux := http.NewServeMux()
	h := &planHandlers{mgr: f.mgr,
		stores: func(*fleet.DatabaseInstance) planService { return f.fake }}
	registerPlanRoutesWith(mux, h, f.tokens, appMode)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user != nil {
			r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
		}
		mux.ServeHTTP(w, r)
	})
}

// planCall sends body (raw JSON; "" for none) with the Authorization
// header authz ("" for none).
func planCall(t *testing.T, h http.Handler, method, path, body,
	authz string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, strings.TrimSpace(w.Body.String())
}

func operatorGrant(id string, scopes []string, databases ...string) mcptoken.Grant {
	return mcptoken.Grant{TokenID: id, Name: "cli-" + id, Kind: mcptoken.KindOperator,
		Scopes: scopes, Databases: databases, OwnerUserID: 31, OwnerRole: auth.RoleOperator}
}
