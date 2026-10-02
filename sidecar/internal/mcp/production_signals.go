package mcp

import "context"

// ListSLOs serves sre_list_slos.
func (backend *ProductionBackend) ListSLOs(ctx context.Context, request SignalRequest) (any,
	error) {
	if backend.dependencies.Signals == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Signals.ListSLOs(ctx, request)
}

// GetSLO serves sre_get_slo.
func (backend *ProductionBackend) GetSLO(ctx context.Context, request SignalRequest) (any,
	error) {
	if backend.dependencies.Signals == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Signals.GetSLO(ctx, request)
}

// ListChanges serves sre_list_changes.
func (backend *ProductionBackend) ListChanges(ctx context.Context,
	request SignalRequest) (any, error) {
	if backend.dependencies.Signals == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Signals.ListChanges(ctx, request)
}
