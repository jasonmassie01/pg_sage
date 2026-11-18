package mcp

import (
	"context"

	"github.com/pg-sage/sidecar/internal/changeplan"
)

// ListPlans serves list_plans.
func (backend *ProductionBackend) ListPlans(ctx context.Context, database string,
	f changeplan.ListFilter) ([]changeplan.Plan, error) {
	if backend.dependencies.Plans == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Plans.ListPlans(ctx, database, f)
}

// GetPlan serves get_plan.
func (backend *ProductionBackend) GetPlan(ctx context.Context, database string,
	id int64) (changeplan.Detail, error) {
	if backend.dependencies.Plans == nil {
		return changeplan.Detail{}, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Plans.GetPlan(ctx, database, id)
}

// ProposePlan serves propose_plan.
func (backend *ProductionBackend) ProposePlan(ctx context.Context, database string,
	p changeplan.Plan, limit int) (changeplan.Plan, error) {
	if backend.dependencies.Plans == nil {
		return changeplan.Plan{}, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Plans.ProposePlan(ctx, database, p, limit)
}
