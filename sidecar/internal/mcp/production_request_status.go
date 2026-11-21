package mcp

import "context"

// RequestStatus serves get_request_status and wait_for_decision (mcp-01b).
func (backend *ProductionBackend) RequestStatus(ctx context.Context, q RequestQuery) (
	RequestRecord, error) {
	if backend.dependencies.RequestStatus == nil {
		return RequestRecord{}, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.RequestStatus.RequestStatus(ctx, q)
}
