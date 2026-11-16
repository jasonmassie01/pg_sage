package mcp

import (
	"context"

	"github.com/pg-sage/sidecar/internal/changeplan"
)

// The plan tools of allToolsBackend (plan-03b).

func init() {
	allToolsArgs["propose_plan"] = `{"plan":` + planJSON(planStep) + `}`
	allToolsArgs["get_plan"] = `{"plan_id":3}`
	allToolsArgs["list_plans"] = `{"status":"proposed"}`
}

func (b *allToolsBackend) ListPlans(ctx context.Context, _ string,
	_ changeplan.ListFilter) ([]changeplan.Plan, error) {
	b.hit(ctx, "list_plans")
	return nil, nil
}

func (b *allToolsBackend) GetPlan(ctx context.Context, _ string,
	id int64) (changeplan.Detail, error) {
	b.hit(ctx, "get_plan")
	return changeplan.Detail{Plan: changeplan.Plan{ID: id}}, nil
}

func (b *allToolsBackend) ProposePlan(ctx context.Context, _ string, p changeplan.Plan,
	_ int) (changeplan.Plan, error) {
	b.hit(ctx, "propose_plan")
	p.ID = 3
	return p, nil
}
