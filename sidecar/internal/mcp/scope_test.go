package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Scoped principals (roadmap phase 3): read, propose and approve. Agent
// principals (agent tokens, stdio) can read and propose; they can never
// approve, whatever scopes they carry. Approve-class tools: deciding facts
// and the declarations that are imported as confirmed facts.

var approveTools = []string{"decide_fact", "declare_table_contract", "register_consumer",
	"sre_downgrade_autonomy", "sre_review_investigation"}

var proposeTools = []string{"propose_policy_change", "request_change", "optimize_query",
	"apply_migration", "ensure_fk_indexes", "set_maintenance_policy", "sre_propose_action",
	"sre_request_execution", "sre_draft_runbook", "sre_compile_runbook",
	"sre_evaluate_autonomy", "propose_fact", "mark_object", "report_source_fix",
	"specialist_request_remediation"}

func agentContext(scopes ...Scope) context.Context {
	return WithPrincipal(context.Background(), Principal{Actor: "token:agt1",
		Kind: KindAgent, Scopes: scopes, TokenID: "agt1"})
}

func TestEveryToolHasARequiredScope(t *testing.T) {
	approve, propose := map[string]bool{}, map[string]bool{}
	for _, name := range approveTools {
		approve[name] = true
	}
	for _, name := range proposeTools {
		propose[name] = true
	}
	for _, tool := range NewServer(&recordingBackend{}).Tools() {
		scope, ok := RequiredScope(tool.Name, nil)
		require.True(t, ok, "%s has no scope", tool.Name)
		switch {
		case approve[tool.Name]:
			require.Equal(t, ScopeApprove, scope, tool.Name)
		case propose[tool.Name]:
			require.Equal(t, ScopePropose, scope, tool.Name)
		default:
			require.Equal(t, ScopeRead, scope, tool.Name)
		}
	}
	_, ok := RequiredScope("execute_sql", nil)
	require.False(t, ok)
}

func TestRequestChangeScopeFollowsTheIntentKind(t *testing.T) {
	for kind, want := range map[string]Scope{
		"optimize_query": ScopePropose, "apply_migration": ScopePropose,
		"register_consumer": ScopeApprove, "declare_table_contract": ScopeApprove,
		"REGISTER_CONSUMER": ScopeApprove, " register_consumer ": ScopeApprove,
	} {
		args := json.RawMessage(`{"intent":{"kind":` + mustJSON(t, kind) + `}}`)
		scope, ok := RequiredScope("request_change", args)
		require.True(t, ok)
		require.Equal(t, want, scope, kind)
	}
	scope, _ := RequiredScope("request_change", json.RawMessage(`{"intent":"x"}`))
	require.Equal(t, ScopePropose, scope)
}

func TestPrincipalScopes(t *testing.T) {
	cases := []struct {
		p    Principal
		want map[Scope]bool
	}{
		{Principal{Actor: "u", Role: "admin"}, map[Scope]bool{ScopeRead: true,
			ScopePropose: true, ScopeApprove: true}},
		{Principal{Actor: "u", Role: "operator"}, map[Scope]bool{ScopeRead: true,
			ScopePropose: true, ScopeApprove: true}},
		{Principal{Actor: "u", Role: "viewer"}, map[Scope]bool{ScopeRead: true}},
		{Principal{Actor: "u", Role: "intern"}, map[Scope]bool{}},
		{Principal{Actor: "a", Kind: KindAgent, Scopes: []Scope{ScopeRead, ScopePropose,
			ScopeApprove}}, map[Scope]bool{ScopeRead: true, ScopePropose: true}},
		{Principal{Actor: "a", Kind: KindAgent, Role: "admin"}, map[Scope]bool{}},
		{Principal{Actor: "t", Scopes: []Scope{ScopeRead}, Role: "admin"},
			map[Scope]bool{ScopeRead: true}},
		{Principal{Actor: "", Role: "admin"}, map[Scope]bool{}},
	}
	for i, c := range cases {
		for _, s := range []Scope{ScopeRead, ScopePropose, ScopeApprove} {
			require.Equal(t, c.want[s], c.p.Has(s), "case %d scope %s", i, s)
		}
	}
}

func TestAgentPrincipalCanNeverApprove(t *testing.T) {
	for _, scopes := range [][]Scope{{ScopeRead, ScopePropose},
		{ScopeRead, ScopePropose, ScopeApprove}} {
		for _, name := range approveTools {
			backend := newAllToolsBackend()
			response := invoke(t, backend.server(), agentContext(scopes...),
				toolCall(name, backend.validArgs(name)))
			require.Equal(t, -32005, response.Error.Code, name)
			require.Contains(t, response.Error.Message, "person", name)
			require.Zero(t, backend.calls(), name)
		}
	}
}

func TestAgentPrincipalReadsAndProposes(t *testing.T) {
	ctx := agentContext(ScopeRead, ScopePropose)
	for _, name := range append([]string{"list_facts", "get_policy", "sre_list_slos",
		"top_queries", "get_source_fix_packet"}, proposeTools...) {
		backend := newAllToolsBackend()
		response := invoke(t, backend.server(), ctx, toolCall(name, backend.validArgs(name)))
		require.Empty(t, response.Error.Code, "%s: %s", name, response.Error.Message)
		require.Equal(t, 1, backend.calls(), name)
	}
}

func TestReadOnlyAgentCannotPropose(t *testing.T) {
	for _, name := range proposeTools {
		backend := newAllToolsBackend()
		response := invoke(t, backend.server(), agentContext(ScopeRead),
			toolCall(name, backend.validArgs(name)))
		require.Equal(t, -32001, response.Error.Code, name)
		require.Contains(t, response.Error.Message, "propose", name)
		require.Zero(t, backend.calls(), name)
	}
}

func TestRequestChangeForAConsumerNeedsApprove(t *testing.T) {
	backend := newAllToolsBackend()
	args := `{"intent":{"kind":"register_consumer","slot_name":"s","owner":"o"}}`
	response := invoke(t, backend.server(), agentContext(ScopeRead, ScopePropose),
		toolCall("request_change", args))
	require.Equal(t, -32005, response.Error.Code)
	require.Zero(t, backend.calls())
	operator := invoke(t, backend.server(), operatorContext(context.Background()),
		toolCall("request_change", args))
	require.Empty(t, operator.Error.Code)
	require.Equal(t, 1, backend.calls())
}

func TestHumanOperatorMayApprove(t *testing.T) {
	for _, name := range approveTools {
		backend := newAllToolsBackend()
		response := invoke(t, backend.server(), operatorContext(context.Background()),
			toolCall(name, backend.validArgs(name)))
		require.Empty(t, response.Error.Code, "%s: %s", name, response.Error.Message)
		require.Equal(t, 1, backend.calls(), name)
	}
}

func TestViewerApproveRefusalIsScopeRequiredNotAgentRefusal(t *testing.T) {
	backend := newAllToolsBackend()
	response := invoke(t, backend.server(), viewerCtx,
		toolCall("decide_fact", backend.validArgs("decide_fact")))
	require.Equal(t, -32001, response.Error.Code)
	require.Contains(t, response.Error.Message, "operator")
}

func TestToolsListShowsOnlyCallableTools(t *testing.T) {
	names := func(ctx context.Context) map[string]bool {
		response := invoke(t, newAllToolsBackend().server(), ctx,
			`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		out := map[string]bool{}
		for _, item := range objectMap(t, response.Result)["tools"].([]any) {
			out[objectMap(t, item)["name"].(string)] = true
		}
		return out
	}
	agent := names(agentContext(ScopeRead, ScopePropose))
	require.True(t, agent["list_facts"])
	require.True(t, agent["propose_fact"])
	for _, name := range approveTools {
		require.False(t, agent[name], name)
	}
	viewer := names(viewerCtx)
	require.True(t, viewer["top_queries"])
	require.False(t, viewer["propose_fact"])
	operator := names(operatorContext(context.Background()))
	require.True(t, operator["decide_fact"])
}

func TestStdioPrincipalIsAnAgent(t *testing.T) {
	require.Equal(t, KindAgent, stdioPrincipal.Kind)
	require.True(t, stdioPrincipal.Has(ScopeRead))
	require.True(t, stdioPrincipal.Has(ScopePropose))
	require.False(t, stdioPrincipal.Has(ScopeApprove))
	backend := newAllToolsBackend()
	response := invoke(t, backend.server(), WithPrincipal(context.Background(),
		stdioPrincipal), toolCall("decide_fact", backend.validArgs("decide_fact")))
	require.Equal(t, -32005, response.Error.Code)
}

func TestTokenActorIsRecorded(t *testing.T) {
	backend := newAllToolsBackend()
	invoke(t, backend.server(), agentContext(ScopeRead, ScopePropose),
		toolCall("propose_fact", backend.validArgs("propose_fact")))
	require.Equal(t, "mcp:token:agt1", backend.facts.actor)
}
