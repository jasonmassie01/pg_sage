package mcp

import "context"

// GetAutonomy serves sre_get_autonomy.
func (backend *ProductionBackend) GetAutonomy(ctx context.Context,
	request AutonomyRequest) (any, error) {
	if backend.dependencies.Autonomy == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Autonomy.GetAutonomy(ctx, request)
}

// DowngradeAutonomy serves sre_downgrade_autonomy.
func (backend *ProductionBackend) DowngradeAutonomy(ctx context.Context,
	request AutonomyRequest, actor string) (any, error) {
	if backend.dependencies.Autonomy == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Autonomy.DowngradeAutonomy(ctx, request, actor)
}
