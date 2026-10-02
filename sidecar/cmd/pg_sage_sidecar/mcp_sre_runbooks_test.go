package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/sre"
)

// The MCP runbook and memory tools resolve databases through the fleet
// like the read tools: drafts are written to the named database's
// runbooks as the bound principal; nothing here signs or runs a runbook.

const mcpRunbookDef = `{"name":"Idle holder","trigger":{"kinds":["lock_blocking"]},` +
	`"start":"read_chains","nodes":[{"id":"read_chains","type":"probe",` +
	`"probe":"lock_chains","next":"esc"},{"id":"esc","type":"proposal",` +
	`"proposal":{"kind":"escalate"}}]}`

func TestMCPRunbookAccess_DraftReadAndSimilar(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	access := &fleetMCPAccess{manager: mgr}
	inv := mcpSREInstance(t, mgr, "mcp_rb")
	ctx := mcp.WithPrincipal(context.Background(), mcp.Principal{Actor: "user:2",
		Role: "operator"})
	out, err := access.DraftRunbook(ctx, mcp.RunbookRequest{
		Definition: json.RawMessage(mcpRunbookDef)})
	rb, ok := out.(sre.Runbook)
	if err != nil || !ok || rb.Status != sre.RunbookDraft || rb.CreatedBy != "mcp:user:2" {
		t.Fatalf("draft = %+v (%v)", out, err)
	}
	revised, err := access.DraftRunbook(ctx, mcp.RunbookRequest{RunbookID: string(rb.ID),
		BaseVersion: 1, Definition: json.RawMessage(strings.Replace(mcpRunbookDef,
			"Idle holder", "Idle holder v2", 1))})
	if r2, _ := revised.(sre.Runbook); err != nil || r2.LatestVersion != 2 {
		t.Fatalf("revise = %+v (%v)", revised, err)
	}
	list, err := access.ListRunbooks(ctx, mcp.RunbookRequest{Database: "mcp_rb"})
	raw, _ := json.Marshal(list)
	if err != nil || !strings.Contains(string(raw), string(rb.ID)) {
		t.Fatalf("list = %s (%v)", raw, err)
	}
	if _, err := access.GetRunbook(ctx, mcp.RunbookRequest{RunbookID: string(rb.ID)}); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := access.RunbookRuns(ctx, mcp.RunbookRequest{RunbookID: string(rb.ID)}); err != nil {
		t.Fatalf("runs: %v", err)
	}
	similar, err := access.SimilarIncidents(ctx, mcp.RunbookRequest{
		InvestigationID: string(inv.ID)})
	if err != nil || similar == nil {
		t.Fatalf("similar = %v (%v)", similar, err)
	}
	if _, err := access.DraftRunbook(ctx, mcp.RunbookRequest{
		Definition: json.RawMessage(`{"name":"x","sql":"DROP"}`)}); !errors.Is(err,
		sre.ErrInvalidRequest) {
		t.Fatalf("draft with an unknown field = %v, want ErrInvalidRequest", err)
	}
	if _, err := access.CompileRunbook(ctx, mcp.RunbookRequest{Text: "read locks"}); !errors.Is(
		err, sre.ErrModelUnavailable) {
		t.Fatalf("compile without a model = %v, want ErrModelUnavailable", err)
	}
}

func TestMCPRunbookAccess_ResolutionErrors(t *testing.T) {
	ctx := context.Background()
	var nilAccess *fleetMCPAccess
	if _, err := nilAccess.ListRunbooks(ctx, mcp.RunbookRequest{}); !errors.Is(err,
		sre.ErrMetadataUnavailable) {
		t.Fatalf("nil access = %v, want ErrMetadataUnavailable", err)
	}
	mgr := fleet.NewManager(config.DefaultConfig())
	access := &fleetMCPAccess{manager: mgr}
	if _, err := access.GetRunbook(ctx, mcp.RunbookRequest{Database: "nope",
		RunbookID: string(sre.NewUUID())}); !errors.Is(err, sre.ErrNotFound) {
		t.Fatalf("unknown database = %v, want ErrNotFound", err)
	}
	if _, err := access.ListRunbooks(ctx, mcp.RunbookRequest{}); !errors.Is(err,
		sre.ErrInvalidRequest) {
		t.Fatalf("no database at all = %v, want ErrInvalidRequest", err)
	}
}
