package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// mcp-09 wiring: the MCP backend says which tool families each monitored
// database can serve, from the Ask Sage registry, the specialist contract
// pointer and the fleet's capability evidence. Unknown evidence counts as
// available, so nothing is hidden on a capability-read error.

// onModel is an enabled LLM that is never called.
type onModel struct{ on bool }

func (onModel) ChatWithTools(context.Context, []llm.Message, []llm.ToolSpec,
	llm.ToolOptions) (llm.ToolResult, error) {
	return llm.ToolResult{}, nil
}

func (m onModel) IsEnabled() bool { return m.on }

func askService(t *testing.T, name string, enabled bool, model *onModel) *ask.Service {
	t.Helper()
	d := ask.Deps{Database: name, Pool: lazyPool(t, name),
		Config: config.AskConfig{Enabled: enabled}}
	if model != nil {
		d.Model = *model
	}
	svc, err := ask.New(d)
	require.NoError(t, err)
	return svc
}

// availabilityFleet is orders (pg_stat_statements available) and billing
// (not installed) plus unknown (no capability evidence yet).
func availabilityFleet() *fleet.DatabaseManager {
	manager := fleet.NewManager(config.DefaultConfig())
	for name, status := range map[string]string{"orders": "available",
		"billing": "not_installed", "unknown": ""} {
		inst := &fleet.DatabaseInstance{Name: name}
		if status != "" {
			s := status
			inst.UpdateStatus(func(st *fleet.InstanceStatus) {
				st.Capabilities.Extensions = map[string]string{"pg_stat_statements": s}
			})
		}
		manager.RegisterInstance(inst)
	}
	return manager
}

func TestMCPAvailabilityAsk(t *testing.T) {
	registry := ask.NewRegistry()
	registry.Set("orders", askService(t, "orders", true, &onModel{on: true}))
	registry.Set("billing", askService(t, "billing", false, &onModel{on: true}))
	registry.Set("nomodel", askService(t, "nomodel", true, nil))
	registry.Set("offmodel", askService(t, "offmodel", true, &onModel{on: false}))
	a := fleetMCPAvailability{asks: registry}
	ctx := context.Background()
	require.True(t, a.Available(ctx, mcp.FamilyAsk, "orders"))
	require.False(t, a.Available(ctx, mcp.FamilyAsk, "billing"), "ask.enabled false")
	require.False(t, a.Available(ctx, mcp.FamilyAsk, "nomodel"), "no LLM configured")
	require.False(t, a.Available(ctx, mcp.FamilyAsk, "offmodel"), "LLM switched off")
	require.False(t, a.Available(ctx, mcp.FamilyAsk, "gone"), "no Ask Sage runtime")
	require.True(t, a.Available(ctx, mcp.FamilyAsk, ""), "unnamed: orders can answer")
	require.False(t, fleetMCPAvailability{}.Available(ctx, mcp.FamilyAsk, "orders"),
		"no registry: Ask Sage cannot answer anywhere")
}

func TestMCPAvailabilitySpecialist(t *testing.T) {
	ctx := context.Background()
	on := fleetMCPAvailability{specialist: func() bool { return true }}
	off := fleetMCPAvailability{specialist: func() bool { return false }}
	require.True(t, on.Available(ctx, mcp.FamilySpecialist, "orders"))
	require.False(t, off.Available(ctx, mcp.FamilySpecialist, "orders"))
	require.False(t, off.Available(ctx, mcp.FamilySpecialist, ""))
	require.True(t, fleetMCPAvailability{}.Available(ctx, mcp.FamilySpecialist, "orders"),
		"no specialist probe wired: unknown counts as available")
}

func TestMCPAvailabilityExtensions(t *testing.T) {
	a := fleetMCPAvailability{manager: availabilityFleet()}
	ctx := context.Background()
	pss := mcp.ExtensionFamily("pg_stat_statements")
	require.True(t, a.Available(ctx, pss, "orders"))
	require.False(t, a.Available(ctx, pss, "billing"), "not installed")
	require.True(t, a.Available(ctx, pss, "unknown"), "no evidence: available")
	require.True(t, a.Available(ctx, pss, "gone"), "no instance: available")
	require.True(t, a.Available(ctx, mcp.ExtensionFamily("hypopg"), "billing"),
		"an extension without evidence is unknown")
	require.True(t, a.Available(ctx, pss, ""), "unnamed in a fleet: unknown")
	require.True(t, fleetMCPAvailability{}.Available(ctx, pss, "orders"), "no manager")
	require.True(t, a.Available(ctx, "a family nobody defined", "billing"))

	single := fleet.NewManager(config.DefaultConfig())
	inst := &fleet.DatabaseInstance{Name: "only"}
	inst.UpdateStatus(func(st *fleet.InstanceStatus) {
		st.Capabilities.Extensions = map[string]string{"pg_stat_statements": "unavailable"}
	})
	single.RegisterInstance(inst)
	require.False(t, fleetMCPAvailability{manager: single}.Available(ctx, pss, ""),
		"unnamed with one database: that database's evidence")
}

// mcpListServer is the production MCP server over manager, with the
// availability wiring the sidecar uses.
func mcpListServer(t *testing.T, manager *fleet.DatabaseManager, registry *ask.Registry,
	specialistOn bool) *mcp.Server {
	t.Helper()
	access := &fleetMCPAccess{manager: manager}
	gate := mcp.NeverApproved(&fleetStandingPolicyGate{manager: manager})
	backend, err := mcp.NewProductionBackend(mcp.ProductionDependencies{
		Gate: gate, Planner: mcp.DeterministicIntentPlanner{},
		Executor: &fleetIntentExecutor{access: access, gate: gate},
		Policy:   access, Ledger: access, Value: access, Guarantees: access,
		AgentTools: fleetAgentTools{manager: manager}, Ask: registry,
		Specialist: processSpecialistMCP{},
		Availability: fleetMCPAvailability{asks: registry, manager: manager,
			specialist: func() bool { return specialistOn }},
	})
	require.NoError(t, err)
	return mcp.NewServer(backend).WithDirectory(fleetMCPDirectory{manager: manager})
}

func listedToolEnums(t *testing.T, server *mcp.Server) map[string][]string {
	t.Helper()
	ctx := mcp.WithPrincipal(context.Background(), mcp.Principal{Actor: "user:1",
		Role: "admin"})
	raw := server.Handle(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	var reply struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties struct {
						Database struct {
							Enum []string `json:"enum"`
						} `json:"database"`
					} `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &reply))
	out := map[string][]string{}
	for _, tool := range reply.Result.Tools {
		out[tool.Name] = tool.InputSchema.Properties.Database.Enum
	}
	return out
}

func TestMCPToolsListFollowsTheSidecarsAvailability(t *testing.T) {
	registry := ask.NewRegistry()
	registry.Set("orders", askService(t, "orders", true, &onModel{on: true}))
	manager := availabilityFleet()
	off := listedToolEnums(t, mcpListServer(t, manager, registry, false))
	require.Equal(t, []string{"orders"}, off["ask_sage"])
	require.Equal(t, []string{"orders", "unknown"}, off["top_queries"])
	require.Equal(t, []string{"orders", "unknown"}, off["whatif_index"])
	require.Equal(t, []string{"billing", "orders", "unknown"}, off["explain_query"])
	require.NotContains(t, off, "specialist_open_investigation")
	on := listedToolEnums(t, mcpListServer(t, manager, registry, true))
	for _, name := range []string{"specialist_open_investigation",
		"specialist_investigation_status", "specialist_investigation_result",
		"specialist_investigation_transcript", "specialist_request_remediation"} {
		require.Contains(t, on, name)
	}
	require.Equal(t, len(off)+5, len(on))
}

// Error propagation: a hidden specialist tool called directly still gets
// the contract's unavailable code (-32010), not "unknown tool".
func TestMCPHiddenSpecialistToolCalledDirectlyIsUnavailable(t *testing.T) {
	prev := specialistProcess.Load()
	specialistProcess.Store(nil)
	t.Cleanup(func() { specialistProcess.Store(prev) })
	server := mcpListServer(t, availabilityFleet(), ask.NewRegistry(), false)
	ctx := mcp.WithPrincipal(context.Background(), mcp.Principal{Actor: "user:1",
		Role: "admin"})
	raw := server.Handle(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":`+
		`"tools/call","params":{"name":"specialist_investigation_status","arguments":`+
		`{"database":"orders","investigation_id":"00000000-0000-0000-0000-000000000001"}}}`))
	require.Contains(t, string(raw), "-32010", string(raw))
	require.NotContains(t, string(raw), "Unknown tool")
}

func TestMCPDependenciesWireAvailability(t *testing.T) {
	prev := cfg
	cfg = config.DefaultConfig()
	t.Cleanup(func() { cfg = prev })
	deps := mcpDependencies()
	a, ok := deps.Availability.(fleetMCPAvailability)
	require.True(t, ok, "the sidecar's MCP backend has no availability")
	require.True(t, a.asks == askServices(), "availability reads the process Ask registry")
	require.True(t, a.specialist != nil, "availability reads the specialist pointer")
	prevRT := specialistProcess.Load()
	t.Cleanup(func() { specialistProcess.Store(prevRT) })
	specialistProcess.Store(nil)
	require.False(t, a.specialist())
	specialistProcess.Store(&specialistRuntime{})
	require.True(t, a.specialist())
}
