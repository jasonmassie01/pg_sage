package mcp

import (
	"context"

	"github.com/pg-sage/sidecar/internal/agenttools"
)

// Facts and the coding-agent tools of allToolsBackend.

func (b *allToolsBackend) ListFacts(ctx context.Context, r FactRequest) (any, error) {
	b.hit(ctx, "list_facts")
	b.facts = factCall{req: r}
	return map[string]any{}, nil
}

func (b *allToolsBackend) ProposeFact(ctx context.Context, r FactRequest, actor string) (any,
	error) {
	b.hit(ctx, "propose_fact")
	b.facts = factCall{req: r, actor: actor}
	return map[string]any{}, nil
}

func (b *allToolsBackend) DecideFact(ctx context.Context, r FactRequest, actor string) (any,
	error) {
	b.hit(ctx, "decide_fact")
	b.facts = factCall{req: r, actor: actor}
	return map[string]any{}, nil
}

func (b *allToolsBackend) recordAgent(ctx context.Context, method string, req any,
	actor string) error {
	b.hit(ctx, method)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.agent = agentCall{method: method, req: req, actor: actor}
	return b.agentErr
}

func (b *allToolsBackend) TopQueries(ctx context.Context, r agenttools.TopQueriesRequest) (
	agenttools.TopQueriesResult, error) {
	return agenttools.TopQueriesResult{Queries: []agenttools.TopQuery{}},
		b.recordAgent(ctx, "top_queries", r, "")
}

func (b *allToolsBackend) ExplainQuery(ctx context.Context, r agenttools.ExplainRequest) (
	agenttools.ExplainResult, error) {
	return agenttools.ExplainResult{Query: r.Query}, b.recordAgent(ctx, "explain_query", r, "")
}

func (b *allToolsBackend) WhatIfIndex(ctx context.Context, r agenttools.WhatIfRequest) (
	agenttools.WhatIfResult, error) {
	return agenttools.WhatIfResult{Available: true}, b.recordAgent(ctx, "whatif_index", r, "")
}

func (b *allToolsBackend) LintMigration(ctx context.Context, r agenttools.LintRequest) (
	agenttools.LintResult, error) {
	return agenttools.LintResult{Verdict: "safe"}, b.recordAgent(ctx, "lint_migration", r, "")
}

func (b *allToolsBackend) QuerySources(ctx context.Context, r agenttools.SourcesRequest) (
	agenttools.SourcesResult, error) {
	return agenttools.SourcesResult{}, b.recordAgent(ctx, "query_sources", r, "")
}

func (b *allToolsBackend) MarkObject(ctx context.Context, r agenttools.MarkRequest,
	actor string) (agenttools.MarkResult, error) {
	return agenttools.MarkResult{Status: "proposed"},
		b.recordAgent(ctx, "mark_object", r, actor)
}

func (b *allToolsBackend) SourceFixPacket(ctx context.Context, findingID int64) (
	agenttools.Packet, error) {
	err := b.recordAgent(ctx, "get_source_fix_packet", findingID, "")
	packet := b.packet
	if packet.FindingID == 0 {
		packet.FindingID = findingID
	}
	return packet, err
}

func (b *allToolsBackend) ReportSourceFix(ctx context.Context, r agenttools.ReportRequest,
	actor string) (agenttools.Report, error) {
	return agenttools.Report{FindingID: r.FindingID, Stage: r.Stage, Verdict: "pending"},
		b.recordAgent(ctx, "report_source_fix", r, actor)
}
