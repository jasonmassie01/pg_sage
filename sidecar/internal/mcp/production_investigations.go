package mcp

import "context"

// ListInvestigations serves sre_list_incidents.
func (backend *ProductionBackend) ListInvestigations(
	ctx context.Context, request InvestigationRequest,
) (any, error) {
	if backend.dependencies.Investigations == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Investigations.ListInvestigations(ctx, request)
}

// GetInvestigation serves sre_get_investigation.
func (backend *ProductionBackend) GetInvestigation(
	ctx context.Context, request InvestigationRequest,
) (any, error) {
	if backend.dependencies.Investigations == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Investigations.GetInvestigation(ctx, request)
}

// GetEvidence serves sre_get_evidence.
func (backend *ProductionBackend) GetEvidence(
	ctx context.Context, request InvestigationRequest,
) (any, error) {
	if backend.dependencies.Investigations == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Investigations.GetEvidence(ctx, request)
}
