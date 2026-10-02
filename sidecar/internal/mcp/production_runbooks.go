package mcp

import "context"

// The production backend serves the runbook tools from its investigations
// dependency when that dependency also serves runbooks.

func (backend *ProductionBackend) runbooks() (RunbookBackend, error) {
	rb, ok := backend.dependencies.Investigations.(RunbookBackend)
	if !ok {
		return nil, ErrProductionDependencyUnavailable
	}
	return rb, nil
}

// ListRunbooks serves sre_list_runbooks.
func (backend *ProductionBackend) ListRunbooks(ctx context.Context,
	request RunbookRequest) (any, error) {
	rb, err := backend.runbooks()
	if err != nil {
		return nil, err
	}
	return rb.ListRunbooks(ctx, request)
}

// GetRunbook serves sre_get_runbook.
func (backend *ProductionBackend) GetRunbook(ctx context.Context,
	request RunbookRequest) (any, error) {
	rb, err := backend.runbooks()
	if err != nil {
		return nil, err
	}
	return rb.GetRunbook(ctx, request)
}

// RunbookRuns serves sre_runbook_runs.
func (backend *ProductionBackend) RunbookRuns(ctx context.Context,
	request RunbookRequest) (any, error) {
	rb, err := backend.runbooks()
	if err != nil {
		return nil, err
	}
	return rb.RunbookRuns(ctx, request)
}

// DraftRunbook serves sre_draft_runbook.
func (backend *ProductionBackend) DraftRunbook(ctx context.Context,
	request RunbookRequest) (any, error) {
	rb, err := backend.runbooks()
	if err != nil {
		return nil, err
	}
	return rb.DraftRunbook(ctx, request)
}

// CompileRunbook serves sre_compile_runbook.
func (backend *ProductionBackend) CompileRunbook(ctx context.Context,
	request RunbookRequest) (any, error) {
	rb, err := backend.runbooks()
	if err != nil {
		return nil, err
	}
	return rb.CompileRunbook(ctx, request)
}

// SimilarIncidents serves sre_similar_incidents.
func (backend *ProductionBackend) SimilarIncidents(ctx context.Context,
	request RunbookRequest) (any, error) {
	rb, err := backend.runbooks()
	if err != nil {
		return nil, err
	}
	return rb.SimilarIncidents(ctx, request)
}
