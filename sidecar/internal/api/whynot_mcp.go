package api

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// WhyNotMCP serves why_not (ux-08b) from the same reads and checks as GET
// /api/v1/whynot, so an agent and the dashboard get the same chain. The
// MCP server has already bound the database the caller may read.
func WhyNotMCP(mgr *fleet.DatabaseManager) mcp.WhyNotBackend {
	return whyNotMCP{mgr: mgr}
}

type whyNotMCP struct{ mgr *fleet.DatabaseManager }

func (b whyNotMCP) WhyNot(ctx context.Context, req mcp.WhyNotRequest) (any, error) {
	values := url.Values{}
	if req.FindingID != 0 {
		values.Set("finding_id", strconv.FormatInt(req.FindingID, 10))
	}
	values.Set("object", req.Object)
	values.Set("category", req.Category)
	q, err := parseWhyNotValues(values)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", mcp.ErrWhyNotInvalidArguments, err.Error())
	}
	if b.mgr == nil {
		return nil, mcp.ErrWhyNotDatabaseNotFound
	}
	selected, ok := resolveSingleDatabaseRequestPool(b.mgr, req.Database)
	if !ok {
		return nil, fmt.Errorf("%w: database is required", mcp.ErrWhyNotInvalidArguments)
	}
	if selected.pool == nil {
		return nil, mcp.ErrWhyNotDatabaseNotFound
	}
	out, err := answerWhyNot(ctx, b.mgr, selected, q)
	if err != nil {
		return nil, err
	}
	return out, nil
}
