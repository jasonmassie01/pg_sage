package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/sre"
)

// sreInvestigatorConfig is the tool-calling investigator of one database
// (roadmap 2.1): on in sre.llm.mode "investigator" (the default), with
// the plan-only EXPLAIN of its monitored database; nil in "review" mode,
// which keeps the M3 single review turn. The default plans apply.
func sreInvestigatorConfig(settings config.SREConfig,
	monitored *pgxpool.Pool) *sre.InvestigatorConfig {
	if settings.LLM.Mode != config.SRELLMModeInvestigator {
		return nil
	}
	cfg := &sre.InvestigatorConfig{}
	if monitored != nil {
		cfg.Explainer = sre.NewStatementExplainer(monitored)
	}
	return cfg
}

var _ mcp.TranscriptBackend = (*fleetMCPAccess)(nil)

// GetTranscript reads one investigation's redacted investigator
// transcript; keep is the operator's opt-in to identifiers.
func (access *fleetMCPAccess) GetTranscript(ctx context.Context,
	request mcp.InvestigationRequest, keep bool) (any, error) {
	svc, err := access.investigationService(request.Database)
	if err != nil {
		return nil, err
	}
	return svc.Transcript(ctx, sre.UUID(request.InvestigationID),
		sre.TranscriptOptions{KeepIdentifiers: keep})
}
