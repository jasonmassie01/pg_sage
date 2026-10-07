package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/specialist"
)

// The transcript MCP tool reaches the contract's Transcript with the
// caller's identity (scope and database checks are the contract's).
func TestSpecialistMCPBackend_Transcript(t *testing.T) {
	cfg := config.DefaultConfig()
	rt, err := buildSpecialistWithStore(cfg, fleet.NewManager(cfg), newSpecialistMemStore(),
		nil, newSpecialistCustodians())
	if err != nil || rt == nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"investigation_id":"44444444-4444-4444-8444-444444444444"}`)
	noRead := mcp.SpecialistCaller{Actor: "token:t-2", TokenID: "t-2", Kind: "agent",
		Scopes: []string{}}
	_, err = rt.mcp.SpecialistCall(context.Background(),
		"specialist_investigation_transcript", noRead, "orders", args)
	if !errors.Is(err, specialist.ErrScope) {
		t.Fatalf("a caller without read: %v", err)
	}
	reader := mcp.SpecialistCaller{Actor: "token:t-1", TokenID: "t-1", Kind: "agent",
		Scopes: []string{"read"}}
	_, err = rt.mcp.SpecialistCall(context.Background(),
		"specialist_investigation_transcript", reader, "orders", args)
	if !errors.Is(err, specialist.ErrNotFound) {
		t.Fatalf("unknown database: %v", err)
	}
}
