package mcp

import "context"

// ListFacts serves list_facts.
func (backend *ProductionBackend) ListFacts(ctx context.Context, request FactRequest) (any,
	error) {
	if backend.dependencies.Facts == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Facts.ListFacts(ctx, request)
}

// ProposeFact serves propose_fact.
func (backend *ProductionBackend) ProposeFact(ctx context.Context, request FactRequest,
	actor string) (any, error) {
	if backend.dependencies.Facts == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Facts.ProposeFact(ctx, request, actor)
}

// DecideFact serves decide_fact.
func (backend *ProductionBackend) DecideFact(ctx context.Context, request FactRequest,
	actor string) (any, error) {
	if backend.dependencies.Facts == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Facts.DecideFact(ctx, request, actor)
}

// FleetFindings serves fleet_findings.
func (backend *ProductionBackend) FleetFindings(ctx context.Context,
	request FleetFindingsRequest) (any, error) {
	if backend.dependencies.FleetLearning == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.FleetLearning.FleetFindings(ctx, request)
}
