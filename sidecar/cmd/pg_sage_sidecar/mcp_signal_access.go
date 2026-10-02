package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// SLO and change-feed MCP tools resolve databases through the fleet like
// the REST API: the named database, else the only one (or every one for
// a list).

var _ mcp.SignalBackend = (*fleetMCPAccess)(nil)

// change-feed list bound.
const mcpChangeLimit = 200

// ListSLOs lists the error-budget state of one or every database's SLOs.
func (access *fleetMCPAccess) ListSLOs(ctx context.Context,
	request mcp.SignalRequest) (any, error) {
	insts, err := access.signalInstances(request.Database)
	if err != nil {
		return nil, err
	}
	items, unavailable := []slo.Status{}, []string{}
	for _, inst := range insts {
		if inst.SLO == nil {
			continue
		}
		sts, err := inst.SLO.Statuses(ctx)
		if err != nil {
			unavailable = append(unavailable, inst.Name)
			continue
		}
		items = append(items, sts...)
	}
	return map[string]any{"slos": items, "unavailable": unavailable}, nil
}

// GetSLO reads one SLO, its history and (with recovery_since) the SLI
// recovery verdict.
func (access *fleetMCPAccess) GetSLO(ctx context.Context,
	request mcp.SignalRequest) (any, error) {
	e, err := access.sloEngine(request.Database, request.Name)
	if err != nil {
		return nil, err
	}
	st, err := e.Status(ctx, request.Name)
	if err != nil {
		return nil, notFound(err)
	}
	trs, err := e.Transitions(ctx, request.Name, 50)
	if err != nil {
		return nil, notFound(err)
	}
	out := map[string]any{"database": e.Database(), "status": st, "transitions": trs}
	if request.RecoverySince != "" {
		since, err := time.Parse(time.RFC3339, request.RecoverySince)
		if err != nil {
			return nil, fmt.Errorf("%w: recovery_since", sre.ErrInvalidRequest)
		}
		rec, err := e.Recovery(ctx, request.Name, since)
		if err != nil {
			return nil, notFound(err)
		}
		out["recovery"] = rec
	}
	return out, nil
}

// ListChanges reads one database's change feed.
func (access *fleetMCPAccess) ListChanges(ctx context.Context,
	request mcp.SignalRequest) (any, error) {
	insts, err := access.signalInstances(request.Database)
	if err != nil {
		return nil, err
	}
	if len(insts) != 1 {
		return nil, fmt.Errorf("%w: name the database (%d are monitored)",
			sre.ErrInvalidRequest, len(insts))
	}
	if insts[0].Changes == nil {
		return nil, sre.ErrMetadataUnavailable
	}
	window := time.Duration(request.WindowMinutes) * time.Minute
	evs, err := insts[0].Changes.Recent(ctx, window, mcpChangeLimit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"database": insts[0].Name, "changes": evs}, nil
}

func (access *fleetMCPAccess) signalInstances(name string) ([]*fleet.DatabaseInstance, error) {
	if access == nil || access.manager == nil {
		return nil, sre.ErrMetadataUnavailable
	}
	if name != "" {
		inst := access.manager.GetInstance(name)
		if inst == nil {
			return nil, fmt.Errorf("%w: database %q", sre.ErrNotFound, name)
		}
		return []*fleet.DatabaseInstance{inst}, nil
	}
	var out []*fleet.DatabaseInstance
	for _, inst := range access.manager.Instances() {
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// sloEngine is the engine of the named SLO: the named database's, or the
// only database that has it.
func (access *fleetMCPAccess) sloEngine(database, name string) (*slo.Engine, error) {
	insts, err := access.signalInstances(database)
	if err != nil {
		return nil, err
	}
	var found []*slo.Engine
	for _, inst := range insts {
		if inst.SLO == nil {
			continue
		}
		for _, o := range inst.SLO.Objectives() {
			if o.Name == name {
				found = append(found, inst.SLO)
			}
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("%w: SLO %q", sre.ErrNotFound, name)
	case 1:
		return found[0], nil
	}
	return nil, fmt.Errorf("%w: %d databases have SLO %q; name the database",
		sre.ErrInvalidRequest, len(found), name)
}

// notFound maps an unknown SLO onto the tools' not-found error.
func notFound(err error) error {
	if errors.Is(err, slo.ErrUnknownSLO) {
		return fmt.Errorf("%w: %v", sre.ErrNotFound, err)
	}
	return err
}
