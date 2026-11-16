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

// WorkloadPackModel serves get_workload_pack.
func (backend *ProductionBackend) WorkloadPackModel(ctx context.Context,
	request WorkloadPackRequest) (any, error) {
	if backend.dependencies.WorkloadPacks == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.WorkloadPacks.WorkloadPackModel(ctx, request)
}

// FleetFindings serves fleet_findings.
func (backend *ProductionBackend) FleetFindings(ctx context.Context,
	request FleetFindingsRequest) (any, error) {
	if backend.dependencies.FleetLearning == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.FleetLearning.FleetFindings(ctx, request)
}

// Inbox serves get_inbox.
func (backend *ProductionBackend) Inbox(ctx context.Context,
	request InboxRequest) (any, error) {
	if backend.dependencies.Inbox == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Inbox.Inbox(ctx, request)
}
