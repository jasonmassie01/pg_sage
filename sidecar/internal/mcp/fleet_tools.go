package mcp

import (
	"context"
	"encoding/json"
)

// Fleet learning (roadmap phase 3). fleet_findings is the one fleet-wide
// read tool: the problems open on several databases with their
// per-database drill-down. It names no single database; a principal
// restricted to some databases only sees those.

// FleetFindingsRequest are the typed arguments of fleet_findings plus the
// databases the caller may see (nil: all).
type FleetFindingsRequest struct {
	MinDatabases int      `json:"min_databases,omitempty"`
	Databases    []string `json:"-"`
}

// FleetLearningBackend serves the fleet learning tools.
type FleetLearningBackend interface {
	FleetFindings(context.Context, FleetFindingsRequest) (any, error)
}

var fleetToolNames = map[string]bool{"fleet_findings": true}

// fleetWideTool reports a tool that is not bound to one database.
func fleetWideTool(name string) bool {
	return name == "list_databases" || fleetToolNames[name] || name == "agent_whoami"
}

func fleetTools() []Tool {
	return []Tool{{Name: "fleet_findings", Description: "List problems open on several " +
		"fleet databases at once (e.g. the same missing index on many tenant " +
		"databases), each once with the databases it affects. Read-only; acting " +
		"still goes through each database's own proposals and approvals",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"min_databases":{"type":"integer","minimum":2,"maximum":10000,` +
			`"description":"databases a problem must be open on (default from ` +
			`fleet_learning.fleet_finding_min_databases)"}},` +
			`"additionalProperties":false}`)}}
}

func (s *Server) callFleetTool(ctx context.Context, _ string,
	raw json.RawMessage) (any, *rpcError) {
	backend, ok := s.backend.(FleetLearningBackend)
	if !ok {
		return nil, failure(codeInternal, "fleet findings unavailable")
	}
	var req FleetFindingsRequest
	if !decodeStrict(raw, &req) || req.MinDatabases < 0 || req.MinDatabases == 1 ||
		req.MinDatabases > 10000 {
		return nil, failure(codeInvalidParams, "invalid arguments: min_databases "+
			"must be an integer from 2 to 10000")
	}
	if p, bound := PrincipalFromContext(ctx); bound && p.Databases != nil {
		req.Databases = append([]string{}, p.Databases...)
	}
	return toolResult(backend.FleetFindings(ctx, req))
}
