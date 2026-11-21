package mcp

import (
	"context"
	"encoding/json"
	"errors"
)

// restore_drills (drill-01d) answers "can this database be restored": for
// each database in the caller's scope, its newest restore drill, its
// newest pass and its status against the 35-day window, with the fleet's
// count; the same read as GET /api/v1/fleet/recoverability. It only reads
// the drill history; it never starts a drill.

const toolRestoreDrills = "restore_drills"

// RestoreDrillsRequest is the database to read ("" = every database the
// caller may read) plus the databases the caller may see (nil: all).
type RestoreDrillsRequest struct {
	Database  string   `json:"database,omitempty"`
	Databases []string `json:"-"`
}

// RestoreDrillsBackend serves restore_drills.
type RestoreDrillsBackend interface {
	RestoreDrills(context.Context, RestoreDrillsRequest) (any, error)
}

// ErrRestoreDrillsDatabaseNotFound is a database that does not exist or
// that the caller may not read: one answer for both.
var ErrRestoreDrillsDatabaseNotFound = errors.New("database not found")

func drillTools() []Tool {
	return []Tool{{Name: toolRestoreDrills,
		InputSchema: json.RawMessage(`{"type":"object","properties":{},` +
			`"additionalProperties":false}`)}}
}

func (s *Server) callDrillTool(ctx context.Context, _ string,
	raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Database string `json:"database"`
	}
	if !decodeStrict(raw, &args) || validateTargetDatabase(args.Database) != nil {
		return nil, failure(codeInvalidParams, "invalid arguments: restore_drills takes "+
			"only database, a monitored database name")
	}
	req := RestoreDrillsRequest{Database: args.Database}
	if p, bound := PrincipalFromContext(ctx); bound && p.Databases != nil {
		req.Databases = append([]string{}, p.Databases...)
	}
	backend, ok := s.backend.(RestoreDrillsBackend)
	if !ok {
		return nil, failure(codeInternal, "restore drill history unavailable")
	}
	result, err := backend.RestoreDrills(ctx, req)
	if errors.Is(err, ErrRestoreDrillsDatabaseNotFound) {
		return nil, failure(codeNotFound, "database not found")
	}
	return toolResult(result, err)
}

// RestoreDrills serves restore_drills.
func (backend *ProductionBackend) RestoreDrills(ctx context.Context,
	request RestoreDrillsRequest) (any, error) {
	if backend.dependencies.RestoreDrills == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.RestoreDrills.RestoreDrills(ctx, request)
}
