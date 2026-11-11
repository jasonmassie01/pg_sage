package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// fleetBriefingsMCP serves the MCP latest-briefing resource (llm-06b) from
// the named database's sage.briefings. The MCP server has already checked
// the read scope and that the principal may use the database.
type fleetBriefingsMCP struct {
	manager *fleet.DatabaseManager
}

func (access fleetBriefingsMCP) LatestBriefing(ctx context.Context, database string) (
	json.RawMessage, error) {
	if access.manager == nil {
		return nil, fmt.Errorf("MCP briefings: %w", mcp.ErrIntentUnavailable)
	}
	pool := access.manager.PoolForDatabase(database)
	if pool == nil {
		return nil, fmt.Errorf("MCP briefings of database %q: no connected pool: %w",
			database, mcp.ErrIntentUnavailable)
	}
	rec, err := briefing.NewStore(pool).Latest(ctx)
	return mcpBriefingRecord(database, rec, err)
}

// mcpBriefingRecord encodes the store's answer for the resource.
func mcpBriefingRecord(database string, rec briefing.Record, err error) (json.RawMessage,
	error) {
	if errors.Is(err, briefing.ErrNoBriefing) {
		return nil, fmt.Errorf("%w: %s", mcp.ErrNoBriefing, briefing.NoBriefingReason)
	}
	if err != nil {
		return nil, fmt.Errorf("MCP briefings of database %q: %w", database, err)
	}
	rec.Database = database
	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("encode the briefing of database %q: %w", database, err)
	}
	return raw, nil
}
