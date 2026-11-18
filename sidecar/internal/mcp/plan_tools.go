package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/pg-sage/sidecar/internal/changeplan"
)

// Plans over MCP (plan-03b). propose_plan (propose scope) stores a plan
// for a person to decide: origin agent and the principal id for an agent
// (a person over MCP is recorded as a person), status proposed, after the
// plan-01a validation, within a per-principal open-plan limit. list_plans
// and get_plan read (read scope). There is no MCP tool that approves,
// rejects or resumes a plan, for anyone: a person decides in the app, the
// CLI or a chat card.

// PlanBackend serves the plan tools on one database's plan store.
type PlanBackend interface {
	ListPlans(ctx context.Context, database string,
		f changeplan.ListFilter) ([]changeplan.Plan, error)
	GetPlan(ctx context.Context, database string, id int64) (changeplan.Detail, error)
	ProposePlan(ctx context.Context, database string, p changeplan.Plan,
		limit int) (changeplan.Plan, error)
}

var planToolNames = map[string]bool{"propose_plan": true, "list_plans": true,
	"get_plan": true}

const (
	defaultPlanList = 50
	maxPlanList     = 500
)

var planStatuses = map[string]bool{"": true, "draft": true, "proposed": true,
	"approved": true, "running": true, "halted": true, "completed": true,
	"reverted": true, "rejected": true, "voided": true, "cancelled": true}

func planTools() []Tool {
	schema := func(properties, required string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":` + properties +
			`,"required":` + required + `,"additionalProperties":false}`)
	}
	db := `"database":{"type":"string","description":"fleet database name"}`
	return []Tool{
		{Name: "propose_plan", Description: "Use when a fix takes several ordered steps. " +
			"Proposes a plan (goal, scope, steps and edges, each step citing a finding_id or " +
			"evidence_ids that resolve) for a person to decide. Returns the stored plan, " +
			"status proposed. Never runs a step and never approves: a person approves in " +
			"pg_sage. At most 5 open plans per principal",
			InputSchema: schema(`{`+db+`,"plan":{"type":["object","string"],`+
				`"description":"the plan as a JSON object (goal, scope, pack_hash, steps, `+
				`edges), or a string holding exactly that JSON, without markdown fences"}}`,
				`["plan"]`)},
		{Name: "list_plans", Description: "Use when you need a database's plans. Returns " +
			"them newest first with their status (proposed ones await a person). Never " +
			"changes a plan; read-only",
			InputSchema: schema(`{`+db+`,"status":{"type":"string"},"limit":{"type":`+
				`"integer","minimum":1,"maximum":500}}`, `[]`)},
		{Name: "get_plan", Description: "Use when you need one plan in full. Returns its " +
			"steps and edges, every approval and what each approval covers now. Never " +
			"changes the plan; read-only",
			InputSchema: schema(`{`+db+`,"plan_id":{"type":"integer","minimum":1}}`,
				`["plan_id"]`)},
	}
}

// planArgs are the typed arguments of the plan tools.
type planArgs struct {
	Database string          `json:"database"`
	PlanID   int64           `json:"plan_id"`
	Status   string          `json:"status"`
	Limit    *int            `json:"limit"`
	Plan     json.RawMessage `json:"plan"`
}

// valid checks a tool's arguments and refuses another tool's.
func (a planArgs) valid(tool string) bool {
	switch tool {
	case "list_plans":
		return a.PlanID == 0 && a.Plan == nil && planStatuses[a.Status] &&
			(a.Limit == nil || (*a.Limit >= 1 && *a.Limit <= maxPlanList))
	case "get_plan":
		return a.PlanID > 0 && a.Plan == nil && a.Status == "" && a.Limit == nil
	default:
		return a.PlanID == 0 && a.Status == "" && a.Limit == nil
	}
}

func (s *Server) callPlanTool(ctx context.Context, name string,
	raw json.RawMessage) (any, *rpcError) {
	var args planArgs
	if !decodeStrict(raw, &args) || !args.valid(name) {
		return nil, failure(codeInvalidParams, "invalid arguments")
	}
	backend, ok := s.backend.(PlanBackend)
	if !ok {
		return nil, failure(codeInternal, "plans unavailable")
	}
	switch name {
	case "list_plans":
		return listPlans(ctx, backend, args)
	case "get_plan":
		d, err := backend.GetPlan(ctx, args.Database, args.PlanID)
		if err != nil {
			return planFailure(ctx, name, err)
		}
		return toolSuccess(map[string]any{"database": args.Database, "plan": d.Plan,
			"approvals": d.Approvals, "step_approvals": d.StepApprovals,
			"coverage": d.Coverage}), nil
	}
	return proposePlan(ctx, backend, args)
}

func listPlans(ctx context.Context, backend PlanBackend, args planArgs) (any, *rpcError) {
	limit := defaultPlanList
	if args.Limit != nil {
		limit = *args.Limit
	}
	plans, err := backend.ListPlans(ctx, args.Database, changeplan.ListFilter{
		Status: changeplan.Status(args.Status), Limit: limit})
	if err != nil {
		return planFailure(ctx, "list_plans", err)
	}
	if plans == nil {
		plans = []changeplan.Plan{}
	}
	return toolSuccess(map[string]any{"database": args.Database, "plans": plans}), nil
}

// planFailure maps a plan store error to a tool error; a refused proposal
// carries its plan_code (and the plan's problems).
func planFailure(ctx context.Context, tool string, err error) (any, *rpcError) {
	var ip *changeplan.InvalidPlanError
	switch {
	case errors.As(err, &ip):
		return planRefusal(codeInvalidParams, "invalid_plan", ip.Error(), ip.Problems), nil
	case errors.Is(err, changeplan.ErrOpenPlanLimit):
		return planRefusal(codeRateLimited, "open_plan_limit", err.Error(), nil), nil
	case errors.Is(err, changeplan.ErrOverCap):
		return planRefusal(codeConflict, "over_cap", err.Error(), nil), nil
	case errors.Is(err, changeplan.ErrInvalid):
		return planRefusal(codeInvalidParams, "invalid_plan", err.Error(), nil), nil
	case errors.Is(err, changeplan.ErrNotFound):
		return nil, failure(codeNotFound, "not found")
	case errors.Is(err, ErrProductionDependencyUnavailable):
		return nil, failure(codeUnavailable, "unavailable: plans are not served here")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, failure(codeCancelled, "request cancelled")
	}
	slog.ErrorContext(ctx, "MCP plan tool failed", "tool", tool, "error", err)
	return nil, failure(codeInternal, "internal error")
}

// planRefusal is an isError result whose error names the plan_code a
// client acts on, with the plan's problems when there are any.
func planRefusal(code int, planCode, message string,
	problems []changeplan.Problem) map[string]any {
	out := toolError(failure(code, message))
	structured, _ := out["structuredContent"].(map[string]any)
	detail, _ := structured["error"].(map[string]any)
	detail["plan_code"] = planCode
	if len(problems) > 0 {
		detail["problems"] = problems
	}
	return out
}
