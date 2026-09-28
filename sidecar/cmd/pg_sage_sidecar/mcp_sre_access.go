package main

import (
	"context"
	"fmt"
	"sort"

	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Sage SRE read tools resolve databases through the fleet, like the REST
// API (AI-SRE-SPEC §9): the named database, the single database when
// there is one, every database for a list. The calling agent never holds
// database credentials.

var _ mcp.InvestigationBackend = (*fleetMCPAccess)(nil)

// ListInvestigations lists one database's, or every database's, latest
// investigations.
func (access *fleetMCPAccess) ListInvestigations(
	ctx context.Context, request mcp.InvestigationRequest,
) (any, error) {
	services, err := access.investigationServices(request.Database)
	if err != nil {
		return nil, err
	}
	items, unavailable, err := sre.ListAcross(ctx, services,
		sre.ListFilter{Limit: 50, CaseID: request.CaseID})
	if err != nil {
		return nil, err
	}
	return map[string]any{"investigations": items, "unavailable": unavailable}, nil
}

// GetInvestigation reads one investigation of one database.
func (access *fleetMCPAccess) GetInvestigation(
	ctx context.Context, request mcp.InvestigationRequest,
) (any, error) {
	svc, err := access.investigationService(request.Database)
	if err != nil {
		return nil, err
	}
	return svc.Detail(ctx, sre.UUID(request.InvestigationID))
}

// GetEvidence reads one evidence item of one investigation.
func (access *fleetMCPAccess) GetEvidence(
	ctx context.Context, request mcp.InvestigationRequest,
) (any, error) {
	svc, err := access.investigationService(request.Database)
	if err != nil {
		return nil, err
	}
	return svc.EvidenceItem(ctx, sre.UUID(request.InvestigationID),
		sre.UUID(request.EvidenceID))
}

// investigationService is the named database's investigator, or the only
// database's when none is named.
func (access *fleetMCPAccess) investigationService(name string) (*sre.Service, error) {
	services, err := access.investigationServices(name)
	switch {
	case err != nil:
		return nil, err
	case len(services) != 1:
		return nil, fmt.Errorf("%w: name the database (%d are monitored)",
			sre.ErrInvalidRequest, len(services))
	}
	return services[0], nil
}

func (access *fleetMCPAccess) investigationServices(name string) ([]*sre.Service, error) {
	if access == nil || access.manager == nil {
		return nil, sre.ErrMetadataUnavailable
	}
	var out []*sre.Service
	if name != "" {
		inst := access.manager.GetInstance(name)
		if inst == nil {
			return nil, fmt.Errorf("%w: database %q", sre.ErrNotFound, name)
		}
		if inst.Investigations == nil {
			return nil, sre.ErrMetadataUnavailable
		}
		return []*sre.Service{inst.Investigations}, nil
	}
	for _, inst := range access.manager.Instances() {
		if inst.Investigations != nil {
			out = append(out, inst.Investigations)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}
