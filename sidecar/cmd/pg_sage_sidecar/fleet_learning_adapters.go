package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/fleetlearn"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/tuning"
)

// fleetPriorSource gives the tuning agents look-alike priors. It reads
// the process's fleet learning service at call time (agents are built
// before it starts); no service, no prior.
type fleetPriorSource struct{}

func (fleetPriorSource) LookalikePrior(ctx context.Context, database, class string,
	tables []string) (*tuning.LookalikePrior, error) {
	svc := fleetLearning
	if svc == nil {
		return nil, nil
	}
	p, ok, err := svc.Prior(ctx, database, class, tables)
	if err != nil || !ok {
		return nil, err
	}
	return &tuning.LookalikePrior{Databases: p.Databases, MinSimilarity: p.MinSimilarity,
		Match: p.Match, Improved: p.Improved, Neutral: p.Neutral,
		Regressed: p.Regressed}, nil
}

// fleetLearningAPI serves the API's look-alike and leader routes.
type fleetLearningAPI struct{}

var _ api.FleetLearningReader = fleetLearningAPI{}

func (fleetLearningAPI) LookAlikes(ctx context.Context, database string) (any, error) {
	svc := fleetLearning
	if svc == nil {
		return []fleetlearn.LookAlike{}, nil
	}
	return svc.LookAlikes(ctx, database)
}

func (fleetLearningAPI) LeaderStatus() any { return fleetLeader.Status() }

// fleetFindingsMCP serves fleet_findings over the fleet, restricted to the
// databases the caller may see.
type fleetFindingsMCP struct{}

var _ mcp.FleetLearningBackend = fleetFindingsMCP{}

func (fleetFindingsMCP) FleetFindings(ctx context.Context,
	req mcp.FleetFindingsRequest) (any, error) {
	minDBs := req.MinDatabases
	if minDBs == 0 && cfg != nil {
		minDBs = cfg.FleetLearning.FleetFindingMinDatabases
	}
	sources := api.FleetSources(fleetMgr)
	if req.Databases != nil {
		allowed := map[string]bool{}
		for _, name := range req.Databases {
			allowed[name] = true
		}
		kept := sources[:0]
		for _, s := range sources {
			if allowed[s.Name] {
				kept = append(kept, s)
			}
		}
		sources = kept
	}
	res := fleetlearn.CollectFleetFindings(ctx, sources, minDBs, 500)
	for _, e := range res.Errors {
		logWarn("mcp", "fleet_findings: %s unreadable: %v", e.Database, e.Cause)
	}
	return res, nil
}
