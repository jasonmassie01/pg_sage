package main

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// Sage SRE action tools resolve the database's action service through the
// fleet. They propose and queue; approval happens in the existing flow.

var _ mcp.SREActionBackend = (*fleetMCPAccess)(nil)

// ProposeAction derives (or returns) an investigation's proposal.
func (access *fleetMCPAccess) ProposeAction(ctx context.Context,
	request mcp.SREActionRequest) (any, error) {
	a, err := access.actionService(request.Database)
	if err != nil {
		return nil, err
	}
	p, err := a.Propose(ctx, sre.UUID(request.InvestigationID), mcp.ActorFromContext(ctx))
	if err != nil {
		return nil, err
	}
	return a.View(ctx, p)
}

// RequestExecution queues a proposal's single approval item.
func (access *fleetMCPAccess) RequestExecution(ctx context.Context,
	request mcp.SREActionRequest) (any, error) {
	a, err := access.actionService(request.Database)
	if err != nil {
		return nil, err
	}
	p, err := a.RequestExecution(ctx, sre.UUID(request.ProposalID),
		mcp.ActorFromContext(ctx))
	if err != nil {
		return nil, err
	}
	return a.View(ctx, p)
}

// actionService is the named database's action service, or the only
// database's when none is named.
func (access *fleetMCPAccess) actionService(name string) (*sreaction.ActionService, error) {
	svc, err := access.investigationService(name)
	if err != nil {
		return nil, err
	}
	inst := access.manager.GetInstance(svc.Name())
	if inst == nil || inst.Actions == nil {
		return nil, fmt.Errorf("%w: actions unavailable for database %q",
			sre.ErrMetadataUnavailable, svc.Name())
	}
	return inst.Actions, nil
}
