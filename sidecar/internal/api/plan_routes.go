package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// Plan routes (plan-03a): read plans and decide them (approve all, up to
// step N or step by step, approve one step, reject, resume, cancel). Each
// plan lives in its monitored database, so ?database= names it (optional
// with one database), as for approval cards. Reads are for any signed-in
// role, decisions for operators; outside a Databricks App each route also
// takes an operator API token as `Authorization: Bearer` (the CLI):
// read scope to read, approve scope to decide (plan_token.go).

// planService is one database's plan store (*changeplan.Store).
type planService interface {
	List(ctx context.Context, f changeplan.ListFilter) ([]changeplan.Plan, error)
	Detail(ctx context.Context, id int64) (changeplan.Detail, error)
	ApproveWith(ctx context.Context, id int64, d changeplan.Decision,
		userID int) (changeplan.Approval, error)
	ApproveStep(ctx context.Context, id, stepID int64, hash string, userID int,
		via changeplan.Via) (changeplan.StepApproval, error)
	Reject(ctx context.Context, id int64, reason string, userID int) error
	Resume(ctx context.Context, id int64, hash string, userID int) error
	Cancel(ctx context.Context, id int64, reason string, userID int) error
}

// planHandlers serves the plan routes over the fleet.
type planHandlers struct {
	mgr    *fleet.DatabaseManager
	stores func(*fleet.DatabaseInstance) planService
}

// databasePlanStore is the database's plan store checking the plan caps
// its standing policy sets.
func databasePlanStore(inst *fleet.DatabaseInstance) planService {
	return changeplan.NewStore(inst.Pool).WithCaps(changeplan.CapsSource(inst.PlanCaps))
}

// registerPlanRoutes registers the plan routes. tokens validates the CLI's
// operator tokens (nil: a Bearer request is 503); in a Databricks App the
// platform owns the Authorization header, so the routes are session-only.
func registerPlanRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	tokens lintTokenValidator, appMode bool) {
	registerPlanRoutesWith(mux, &planHandlers{mgr: mgr, stores: databasePlanStore},
		tokens, appMode)
}

func registerPlanRoutesWith(mux *http.ServeMux, h *planHandlers, tokens lintTokenValidator,
	appMode bool) {
	reads := []struct {
		pattern string
		read    func(*planHandlers, http.ResponseWriter, *http.Request)
	}{
		{"GET /api/v1/plans", (*planHandlers).list},
		{"GET /api/v1/plans/{id}", (*planHandlers).get},
	}
	for _, rd := range reads {
		build := func(m *fleet.DatabaseManager) http.Handler {
			v := &planHandlers{mgr: m, stores: h.stores}
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rd.read(v, w, r)
			})
		}
		inner := requireReadScope(readScope{kind: readKindAggregate})(aggregateRead(h.mgr, build))
		registerPlanRoute(mux, rd.pattern, gateViewer, "", inner, tokens, appMode,
			mcptoken.ScopeRead)
		recordReadScope(mux, rd.pattern, readKindAggregate)
	}
	res := approvalCardDatabase(h.mgr)
	decisions := []struct {
		pattern string
		decide  http.HandlerFunc
	}{
		{"POST /api/v1/plans/{id}/approve", h.approve},
		{"POST /api/v1/plans/{id}/steps/{step}/approve", h.approveStep},
		{"POST /api/v1/plans/{id}/reject", h.reject},
		{"POST /api/v1/plans/{id}/resume", h.resume},
		{"POST /api/v1/plans/{id}/cancel", h.cancel},
	}
	for _, d := range decisions {
		inner := requireDatabaseRole(auth.RoleOperator, res)(d.decide)
		registerPlanRoute(mux, d.pattern, gateOperator, "database:"+res.name, inner, tokens,
			appMode, mcptoken.ScopeApprove)
	}
}

// registerPlanRoute registers h behind gate, taking an operator token with
// scope instead of a session outside a Databricks App.
func registerPlanRoute(mux *http.ServeMux, pattern string, gate routeGate, scope string,
	h http.Handler, tokens lintTokenValidator, appMode bool, tokenScope string) {
	if !appMode {
		h = newPlanTokenOrSession(gate, tokens, tokenScope, h)
	}
	registerRoute(mux, pattern, gate, scope, h)
}

// tokenMayUse reports whether a token caller may use database name (a
// session caller always may; its grants are checked by the route).
func tokenMayUse(r *http.Request, name string) bool {
	p, ok := mcp.PrincipalFromContext(r.Context())
	return !ok || p.MayUseDatabase(name)
}

var planStatuses = map[string]bool{"": true, "draft": true, "proposed": true,
	"approved": true, "running": true, "halted": true, "completed": true,
	"reverted": true, "rejected": true, "voided": true, "cancelled": true}

// planListItem is one plan of the list, with its database.
type planListItem struct {
	Database string `json:"database"`
	changeplan.Plan
}

func (h *planHandlers) list(w http.ResponseWriter, r *http.Request) {
	db, ok := readDatabaseParam(w, r)
	if !ok || rejectUnknownDatabase(w, h.mgr, db) {
		return
	}
	status := r.URL.Query().Get("status")
	if !planStatuses[status] {
		sreErrorCode(w, "unknown plan status "+strconv.Quote(status), "invalid_request",
			http.StatusBadRequest)
		return
	}
	limit := parseIntDefault(r.URL.Query().Get("limit"), 100)
	if limit < 1 || limit > 500 {
		sreErrorCode(w, "limit must be 1 to 500", "invalid_request", http.StatusBadRequest)
		return
	}
	plans := make([]planListItem, 0)
	failures := make([]map[string]string, 0)
	for _, inst := range sortedFleetInstances(h.mgr) {
		if inst == nil || inst.Pool == nil || (db != "" && db != "all" && inst.Name != db) ||
			!tokenMayUse(r, inst.Name) {
			continue
		}
		got, err := h.stores(inst).List(r.Context(), changeplan.ListFilter{
			Status: changeplan.Status(status), Limit: limit})
		if err != nil {
			failures = append(failures, fleetReadFailure(inst.Name, "list plans", err))
			continue
		}
		for _, p := range got {
			plans = append(plans, planListItem{Database: inst.Name, Plan: p})
		}
	}
	jsonResponse(w, map[string]any{"plans": plans, "total": len(plans), "errors": failures})
}

// planTarget resolves {id} and ?database= (optional with one database).
// A database a token may not use answers as an unknown one.
func (h *planHandlers) planTarget(w http.ResponseWriter, r *http.Request) (
	*fleet.DatabaseInstance, int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		sreErrorCode(w, "invalid plan id", "invalid_request", http.StatusBadRequest)
		return nil, 0, false
	}
	db, ok := readDatabaseParam(w, r)
	if !ok {
		return nil, 0, false
	}
	instances := sortedFleetInstances(h.mgr)
	if db == "" || db == "all" {
		if len(instances) != 1 {
			sreErrorCode(w, "database is required", "invalid_request", http.StatusBadRequest)
			return nil, 0, false
		}
		db = instances[0].Name
	}
	inst := h.mgr.GetInstance(db)
	if inst == nil || inst.Pool == nil || !tokenMayUse(r, db) {
		sreErrorCode(w, "database not found", "not_found", http.StatusNotFound)
		return nil, 0, false
	}
	return inst, id, true
}

func (h *planHandlers) get(w http.ResponseWriter, r *http.Request) {
	inst, id, ok := h.planTarget(w, r)
	if !ok {
		return
	}
	d, err := h.stores(inst).Detail(r.Context(), id)
	if err != nil {
		writePlanError(w, r, "read plan", err)
		return
	}
	jsonResponse(w, planDetailBody(inst.Name, d))
}

// planDetailBody is the plan detail response; lists are never null.
func planDetailBody(database string, d changeplan.Detail) map[string]any {
	if d.Plan.Steps == nil {
		d.Plan.Steps = []changeplan.Step{}
	}
	if d.Plan.Edges == nil {
		d.Plan.Edges = []changeplan.Edge{}
	}
	return map[string]any{"database": database, "plan": d.Plan,
		"plan_hash": d.Plan.PlanHash, "approvals": nonNilList(d.Approvals),
		"step_approvals": nonNilList(d.StepApprovals), "coverage": nonNilList(d.Coverage)}
}

func nonNilList[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}
