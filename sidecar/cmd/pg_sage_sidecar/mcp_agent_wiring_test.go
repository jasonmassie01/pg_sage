package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// MCP v2 wiring: the fleet is the MCP directory, a database named by the
// server (in the request context) selects the instance for every backend,
// and the coding-agent tools run on that instance's pool only.

func TestFleetMCPDirectoryListsInstancesSortedByName(t *testing.T) {
	manager := fleet.NewManager(config.DefaultConfig())
	manager.RegisterInstance(mcpTestInstance("orders", 41, &mcpGateRecorder{}))
	manager.RegisterInstance(mcpTestInstance("analytics", 0, &mcpGateRecorder{}))
	refs := fleetMCPDirectory{manager: manager}.Databases()
	require.Equal(t, []mcp.DatabaseRef{{Name: "analytics", ID: 0}, {Name: "orders", ID: 41}},
		refs)
	require.Empty(t, fleetMCPDirectory{}.Databases())
}

func TestMCPInstancePrefersTheContextDatabase(t *testing.T) {
	manager := fleet.NewManager(config.DefaultConfig())
	manager.RegisterInstance(mcpTestInstance("orders", 0, &mcpGateRecorder{}))
	manager.RegisterInstance(mcpTestInstance("billing", 0, &mcpGateRecorder{}))
	ctx := mcp.WithDatabase(context.Background(), "billing")
	require.Equal(t, "billing", mcpInstanceFor(ctx, manager, nil).Name)
	zero := int64(0)
	require.Equal(t, "billing", mcpInstanceFor(ctx, manager, &zero).Name,
		"static fleets have database id 0 everywhere: the name decides")
	require.Nil(t, mcpInstanceFor(mcp.WithDatabase(context.Background(), "gone"), manager, nil))
	require.Nil(t, mcpInstanceFor(context.Background(), manager, nil),
		"untargeted multi-instance requests fail closed")
}

func TestMCPStandingGateUsesTheContextDatabase(t *testing.T) {
	manager := fleet.NewManager(config.DefaultConfig())
	orders := &mcpGateRecorder{decision: policy.Decision{Verdict: policy.VerdictPark,
		EvidenceID: "orders"}}
	billing := &mcpGateRecorder{decision: policy.Decision{Verdict: policy.VerdictPark,
		EvidenceID: "billing"}}
	manager.RegisterInstance(mcpTestInstance("orders", 0, orders))
	manager.RegisterInstance(mcpTestInstance("billing", 0, billing))
	decision := (&fleetStandingPolicyGate{manager: manager}).Authorize(
		mcp.WithDatabase(context.Background(), "billing"), policy.ActionRequest{})
	require.Equal(t, "billing", decision.EvidenceID)
	require.Zero(t, orders.calls)
	require.Equal(t, 1, billing.calls)
}

func TestFleetAgentToolsNeedAResolvedDatabaseWithAPool(t *testing.T) {
	manager := fleet.NewManager(config.DefaultConfig())
	manager.RegisterInstance(mcpTestInstance("orders", 0, &mcpGateRecorder{}))
	manager.RegisterInstance(mcpTestInstance("billing", 0, &mcpGateRecorder{}))
	backend := fleetAgentTools{manager: manager}
	_, err := backend.TopQueries(context.Background(), agenttools.TopQueriesRequest{})
	require.True(t, errors.Is(err, agenttools.ErrInvalid), "no database named: %v", err)
	_, err = backend.TopQueries(mcp.WithDatabase(context.Background(), "gone"),
		agenttools.TopQueriesRequest{})
	require.True(t, errors.Is(err, agenttools.ErrNotFound), "unknown database: %v", err)
	_, err = backend.TopQueries(mcp.WithDatabase(context.Background(), "orders"),
		agenttools.TopQueriesRequest{})
	require.True(t, errors.Is(err, agenttools.ErrUnavailable), "instance without pool: %v",
		err)
}
