package mcp

import (
	"context"

	"github.com/pg-sage/sidecar/internal/agenttools"
)

// The coding-agent tools delegate to ProductionDependencies.AgentTools,
// which resolves the database the server bound to the request.

func (backend *ProductionBackend) agentTools() (AgentToolBackend, error) {
	if backend.dependencies.AgentTools == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.AgentTools, nil
}

// TopQueries serves top_queries.
func (backend *ProductionBackend) TopQueries(ctx context.Context,
	request agenttools.TopQueriesRequest) (agenttools.TopQueriesResult, error) {
	tools, err := backend.agentTools()
	if err != nil {
		return agenttools.TopQueriesResult{}, err
	}
	return tools.TopQueries(ctx, request)
}

// ExplainQuery serves explain_query.
func (backend *ProductionBackend) ExplainQuery(ctx context.Context,
	request agenttools.ExplainRequest) (agenttools.ExplainResult, error) {
	tools, err := backend.agentTools()
	if err != nil {
		return agenttools.ExplainResult{}, err
	}
	return tools.ExplainQuery(ctx, request)
}

// WhatIfIndex serves whatif_index.
func (backend *ProductionBackend) WhatIfIndex(ctx context.Context,
	request agenttools.WhatIfRequest) (agenttools.WhatIfResult, error) {
	tools, err := backend.agentTools()
	if err != nil {
		return agenttools.WhatIfResult{}, err
	}
	return tools.WhatIfIndex(ctx, request)
}

// LintMigration serves lint_migration.
func (backend *ProductionBackend) LintMigration(ctx context.Context,
	request agenttools.LintRequest) (agenttools.LintResult, error) {
	tools, err := backend.agentTools()
	if err != nil {
		return agenttools.LintResult{}, err
	}
	return tools.LintMigration(ctx, request)
}

// QuerySources serves query_sources.
func (backend *ProductionBackend) QuerySources(ctx context.Context,
	request agenttools.SourcesRequest) (agenttools.SourcesResult, error) {
	tools, err := backend.agentTools()
	if err != nil {
		return agenttools.SourcesResult{}, err
	}
	return tools.QuerySources(ctx, request)
}

// MarkObject serves mark_object.
func (backend *ProductionBackend) MarkObject(ctx context.Context,
	request agenttools.MarkRequest, actor string) (agenttools.MarkResult, error) {
	tools, err := backend.agentTools()
	if err != nil {
		return agenttools.MarkResult{}, err
	}
	return tools.MarkObject(ctx, request, actor)
}

// SourceFixPacket serves get_source_fix_packet.
func (backend *ProductionBackend) SourceFixPacket(ctx context.Context,
	findingID int64) (agenttools.Packet, error) {
	tools, err := backend.agentTools()
	if err != nil {
		return agenttools.Packet{}, err
	}
	return tools.SourceFixPacket(ctx, findingID)
}

// ReportSourceFix serves report_source_fix.
func (backend *ProductionBackend) ReportSourceFix(ctx context.Context,
	request agenttools.ReportRequest, actor string) (agenttools.Report, error) {
	tools, err := backend.agentTools()
	if err != nil {
		return agenttools.Report{}, err
	}
	return tools.ReportSourceFix(ctx, request, actor)
}
