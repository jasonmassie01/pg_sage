package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// The agent MCP tools from three workstreams must all reach the server:
// a merge once dropped the broker's (agent_query, agent_whoami).
func TestMCPDependenciesWireTheAgentTools(t *testing.T) {
	prev := cfg
	cfg = config.DefaultConfig()
	t.Cleanup(func() { cfg = prev })
	deps := mcpDependencies()
	if deps.AgentBroker == nil {
		t.Fatal("agent_query and agent_whoami have no backend")
	}
	if deps.Grants == nil {
		t.Fatal("agent_request_capability has no backend")
	}
}
