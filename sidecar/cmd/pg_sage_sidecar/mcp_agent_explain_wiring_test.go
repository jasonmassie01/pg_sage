package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard/readapi"
	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// security-02 wiring: agents' explain_query is served by the broker as the
// agent's own role (agentBrokerMCP.AgentExplain), the coding-agent tools
// expose the agent path on the resolved database, and explain.enabled is
// read through the live config struct on every call.

var (
	_ mcp.AgentExplainBackend = agentBrokerMCP{}
	_ mcp.AgentExplainTools   = fleetAgentTools{}
)

func TestAgentToolOptionsShareTheLiveExplainConfig(t *testing.T) {
	c := config.DefaultConfig()
	opts := agentToolOptions(c)
	require.True(t, opts.Explain == &c.Explain, "explain options are a copy, not the config")
	c.Explain.Enabled = false
	_, err := agenttools.New(nil, opts).ExplainQuery(context.Background(),
		agenttools.ExplainRequest{Query: "SELECT 1"})
	require.ErrorIs(t, err, agenttools.ErrUnavailable)
	require.Contains(t, err.Error(), "explain.enabled")
}

func TestFleetAgentExplainNeedsAResolvedDatabase(t *testing.T) {
	manager := fleet.NewManager(config.DefaultConfig())
	manager.RegisterInstance(mcpTestInstance("orders", 0, &mcpGateRecorder{}))
	backend := fleetAgentTools{manager: manager, options: agentToolOptions(
		config.DefaultConfig())}
	called := false
	plan := func(context.Context, readapi.ExplainRequest) (readapi.ExplainResult, error) {
		called = true
		return readapi.ExplainResult{}, nil
	}
	_, err := backend.ExplainAsAgent(context.Background(),
		agenttools.ExplainRequest{Query: "SELECT 1"}, plan)
	require.True(t, errors.Is(err, agenttools.ErrInvalid), "no database: %v", err)
	_, err = backend.ExplainAsAgent(mcp.WithDatabase(context.Background(), "orders"),
		agenttools.ExplainRequest{Query: "SELECT 1"}, plan)
	require.True(t, errors.Is(err, agenttools.ErrUnavailable), "no pool: %v", err)
	require.False(t, called, "the planner ran without a database to plan on")
}

func TestAgentBrokerExplainUnavailableWithoutGovernance(t *testing.T) {
	agentBrokerMu.Lock()
	prev := agentBrokerState
	agentBrokerState = nil
	agentBrokerMu.Unlock()
	prevCfg, prevMgr, prevMeta, prevCtx := cfg, fleetMgr, globalMetaState, shutdownCtx
	cfg, fleetMgr, globalMetaState, shutdownCtx = nil, nil, nil, context.Background()
	t.Cleanup(func() {
		agentBrokerMu.Lock()
		agentBrokerState = prev
		agentBrokerMu.Unlock()
		cfg, fleetMgr, globalMetaState, shutdownCtx = prevCfg, prevMgr, prevMeta, prevCtx
	})
	_, err := agentBrokerMCP{}.AgentExplain(context.Background(),
		readapi.ExplainRequest{Database: "orders", SQL: "SELECT 1"})
	require.ErrorIs(t, err, readapi.ErrUnavailable)
}
