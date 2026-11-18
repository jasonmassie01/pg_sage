package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/mcp"
)

// ux-08b: why_not over MCP answers from the route's own reads. These
// tests need no database: every refusal happens before one is read.

func TestWhyNotMCPRefusesWhatTheRouteRefuses(t *testing.T) {
	b := WhyNotMCP(poollessFleet())
	for _, c := range []struct {
		req   mcp.WhyNotRequest
		want  error
		field string
	}{
		{mcp.WhyNotRequest{Database: "prod", Object: "public"},
			mcp.ErrWhyNotInvalidArguments, "object"},
		{mcp.WhyNotRequest{Database: "prod", Object: "public.t;drop"},
			mcp.ErrWhyNotInvalidArguments, "object"},
		{mcp.WhyNotRequest{Database: "prod", Category: "Bad Category"},
			mcp.ErrWhyNotInvalidArguments, "category"},
		{mcp.WhyNotRequest{Database: "prod", FindingID: 3, Object: "public.t"},
			mcp.ErrWhyNotInvalidArguments, "finding_id"},
		{mcp.WhyNotRequest{Database: "prod", FindingID: -1},
			mcp.ErrWhyNotInvalidArguments, "finding_id"},
		{mcp.WhyNotRequest{}, mcp.ErrWhyNotInvalidArguments, "database"},
		{mcp.WhyNotRequest{Database: "nope"}, mcp.ErrWhyNotDatabaseNotFound, ""},
		{mcp.WhyNotRequest{Database: "prod"}, mcp.ErrWhyNotDatabaseNotFound, ""},
	} {
		got, err := b.WhyNot(context.Background(), c.req)
		if got != nil || !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%+v: %v, %v; want %v naming %q", c.req, got, err, c.want, c.field)
		}
	}
	if _, err := WhyNotMCP(nil).WhyNot(context.Background(),
		mcp.WhyNotRequest{Database: "prod"}); !errors.Is(err, mcp.ErrWhyNotDatabaseNotFound) {
		t.Errorf("no fleet manager: %v, want database not found", err)
	}
}
