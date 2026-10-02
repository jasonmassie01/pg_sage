package main

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Sage SRE runbook and memory tools resolve databases through the fleet,
// like the read tools. Drafts are written as the bound MCP principal;
// nothing here signs or runs a runbook.

var _ mcp.RunbookBackend = (*fleetMCPAccess)(nil)

// ListRunbooks lists one database's runbooks.
func (access *fleetMCPAccess) ListRunbooks(ctx context.Context,
	request mcp.RunbookRequest) (any, error) {
	svc, err := access.investigationService(request.Database)
	if err != nil {
		return nil, err
	}
	items, err := svc.Runbooks(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"database": svc.Name(), "runbooks": items}, nil
}

// GetRunbook reads one runbook with every version.
func (access *fleetMCPAccess) GetRunbook(ctx context.Context,
	request mcp.RunbookRequest) (any, error) {
	svc, err := access.investigationService(request.Database)
	if err != nil {
		return nil, err
	}
	return svc.Runbook(ctx, sre.UUID(request.RunbookID))
}

// RunbookRuns lists a runbook's runs.
func (access *fleetMCPAccess) RunbookRuns(ctx context.Context,
	request mcp.RunbookRequest) (any, error) {
	svc, err := access.investigationService(request.Database)
	if err != nil {
		return nil, err
	}
	runs, err := svc.RunbookRuns(ctx, sre.UUID(request.RunbookID))
	if err != nil {
		return nil, err
	}
	return map[string]any{"runs": runs}, nil
}

// DraftRunbook creates a runbook draft, or appends a draft version on top
// of request.BaseVersion.
func (access *fleetMCPAccess) DraftRunbook(ctx context.Context,
	request mcp.RunbookRequest) (any, error) {
	svc, err := access.investigationService(request.Database)
	if err != nil {
		return nil, err
	}
	def, err := runbook.Decode(request.Definition)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", sre.ErrInvalidRequest, err)
	}
	actor := mcp.ActorFromContext(ctx)
	if request.RunbookID == "" {
		return svc.CreateRunbook(ctx, def, actor)
	}
	return svc.ReviseRunbook(ctx, sre.UUID(request.RunbookID), request.BaseVersion, def,
		actor)
}

// CompileRunbook compiles an English playbook into a draft.
func (access *fleetMCPAccess) CompileRunbook(ctx context.Context,
	request mcp.RunbookRequest) (any, error) {
	svc, err := access.investigationService(request.Database)
	if err != nil {
		return nil, err
	}
	return svc.CompileRunbook(ctx, request.Text, mcp.ActorFromContext(ctx))
}

// SimilarIncidents lists an investigation's similar past incidents.
func (access *fleetMCPAccess) SimilarIncidents(ctx context.Context,
	request mcp.RunbookRequest) (any, error) {
	svc, err := access.investigationService(request.Database)
	if err != nil {
		return nil, err
	}
	items, err := svc.Similar(ctx, sre.UUID(request.InvestigationID))
	if err != nil {
		return nil, err
	}
	return map[string]any{"label": sre.SimilarLabel, "incidents": items}, nil
}
