package mcp

import (
	"context"
	"encoding/json"
)

// LatestBriefing serves the latest-briefing resource (llm-06b).
func (backend *ProductionBackend) LatestBriefing(ctx context.Context, database string) (
	json.RawMessage, error) {
	if backend.dependencies.Briefings == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Briefings.LatestBriefing(ctx, database)
}
