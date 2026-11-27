package mcp

import "context"

// The statistics maintenance tools (playbooks-06) join the all-tools
// catalog: valid arguments for the strict and scope sweeps, their scope
// class, and allToolsBackend methods that count the calls.

func init() {
	for tool, args := range maintenanceToolArgs {
		allToolsArgs[tool] = args
	}
	proposeTools = append(proposeTools, "sre_refresh_table_evidence",
		"sre_propose_statistics", "sre_request_statistics")
	approveTools = append(approveTools, "sre_rollback_statistics")
}

func (b *allToolsBackend) RefreshTableEvidence(ctx context.Context,
	_ SREMaintenanceRequest) (any, error) {
	b.hit(ctx, "sre_refresh_table_evidence")
	return map[string]any{}, nil
}

func (b *allToolsBackend) ProposeStatistics(ctx context.Context,
	_ SREMaintenanceRequest) (any, error) {
	b.hit(ctx, "sre_propose_statistics")
	return map[string]any{}, nil
}

func (b *allToolsBackend) RequestStatistics(ctx context.Context,
	_ SREMaintenanceRequest) (any, error) {
	b.hit(ctx, "sre_request_statistics")
	return map[string]any{}, nil
}

func (b *allToolsBackend) RollbackStatistics(ctx context.Context,
	_ SREMaintenanceRequest) (any, error) {
	b.hit(ctx, "sre_rollback_statistics")
	return map[string]any{}, nil
}
