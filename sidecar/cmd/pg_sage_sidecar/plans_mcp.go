package main

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// plansMCPBackend serves the MCP plan tools (plan-03b) from each
// database's plan store: proposals are validated against the database's
// findings and workload packs and checked against the plan caps of its
// standing policy. It only reads and proposes; nothing here approves.
type plansMCPBackend struct{ manager *fleet.DatabaseManager }

func (b plansMCPBackend) store(database string) (*changeplan.Store, *fleet.DatabaseInstance,
	error) {
	if b.manager == nil {
		return nil, nil, fmt.Errorf("%w: no monitored databases", changeplan.ErrNotFound)
	}
	if database == "" {
		instances := b.manager.Instances()
		if len(instances) != 1 {
			return nil, nil, fmt.Errorf("%w: database is required", changeplan.ErrInvalid)
		}
		for name := range instances {
			database = name
		}
	}
	inst := b.manager.GetInstance(database)
	if inst == nil || inst.Pool == nil {
		return nil, nil, fmt.Errorf("%w: database %q is not monitored",
			changeplan.ErrNotFound, database)
	}
	return changeplan.NewStore(inst.Pool).WithCaps(changeplan.CapsSource(inst.PlanCaps)),
		inst, nil
}

func (b plansMCPBackend) ListPlans(ctx context.Context, database string,
	f changeplan.ListFilter) ([]changeplan.Plan, error) {
	s, _, err := b.store(database)
	if err != nil {
		return nil, err
	}
	return s.List(ctx, f)
}

func (b plansMCPBackend) GetPlan(ctx context.Context, database string,
	id int64) (changeplan.Detail, error) {
	s, _, err := b.store(database)
	if err != nil {
		return changeplan.Detail{}, err
	}
	return s.Detail(ctx, id)
}

func (b plansMCPBackend) ProposePlan(ctx context.Context, database string, p changeplan.Plan,
	limit int) (changeplan.Plan, error) {
	s, inst, err := b.store(database)
	if err != nil {
		return changeplan.Plan{}, err
	}
	return s.ProposeLimited(ctx, p, changeplan.NewDBResolver(inst.Pool, inst.Pool), limit)
}
