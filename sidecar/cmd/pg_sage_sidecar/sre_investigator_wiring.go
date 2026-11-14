package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sreInvestigatorConfig is the tool-calling investigator of one database
// (roadmap 2.1): on in sre.llm.mode "investigator" (the default), with
// the plan-only EXPLAIN of its monitored database inside its probe
// runner's slots (texts: the collector's in-memory statement texts); nil
// in "review" mode, which keeps the M3 single review turn. The default
// plans apply; sre.investigator.require_support_citation sets the
// Support-evidence rule.
func sreInvestigatorConfig(settings config.SREConfig, runner *probes.Runner,
	texts sre.StatementTexts) *sre.InvestigatorConfig {
	if settings.LLM.Mode != config.SRELLMModeInvestigator {
		return nil
	}
	cfg := &sre.InvestigatorConfig{
		RequireSupportCitation: settings.Investigator.RequireSupportCitation}
	if runner.Pool() != nil {
		cfg.Explainer = sre.NewStatementExplainer(runner, texts)
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
