package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// get_request_status (mcp-01b): a held item's state now, scoped to the
// principal that made the request.

// No state transition tests here: get_request_status reports one read;
// the transitions a waiter sees are in wait_for_decision_test.go.

func TestRequestStatusReportsTheItemsStateAndNext(t *testing.T) {
	backend := newStatusBackend()
	backend.set("change_approval:7", RequestRecord{State: "approved",
		DecidedByKind: "person", Outcome: "approved by a person",
		OwnerPrincipalID: agpID("a1")})
	content := mustSucceed(t, callTool(waitServer(backend, nil), waitAgentCtx("a1"),
		toolGetRequestStatus, `{"handle":"change_approval:7"}`))
	require.Equal(t, "change_approval:7", content["handle"])
	require.Equal(t, "orders", content["database"])
	require.Equal(t, "approved", content["state"])
	require.Equal(t, "person", content["decided_by_kind"])
	require.Equal(t, "approved by a person", content["outcome"])
	next := objectMap(t, content["next"])
	require.Equal(t, toolWaitForDecision, next["tool"], "approved is not final")
	require.Equal(t, map[string]any{"handle": "change_approval:7", "database": "orders"},
		next["arguments"])
	require.Equal(t, 1, backend.readCount(), "one read, no wait")
}

func TestRequestStatusOfAFinalItemHasNoNext(t *testing.T) {
	backend := newStatusBackend()
	backend.set("plan:3", RequestRecord{State: "rejected", DecidedByKind: "person",
		Final: true, OwnerActor: "mcp:token:a1"})
	content := mustSucceed(t, callTool(waitServer(backend, nil), waitAgentCtx("a1"),
		toolGetRequestStatus, `{"handle":"plan:3"}`))
	require.Equal(t, "rejected", content["state"])
	require.Contains(t, content, "next", "next is always present")
	require.Nil(t, content["next"], "nothing more will happen to a rejected plan")
}

// badHandleArguments are calls with a malformed handle or extra keys.
var badHandleArguments = []string{`{}`, `{"handle":""}`, `{"handle":7}`,
	`{"handle":"plan"}`, `{"handle":"plan:"}`, `{"handle":":4"}`, `{"handle":"plan:abc"}`,
	`{"handle":"plan:0"}`, `{"handle":"plan:-3"}`, `{"handle":"plan:1.5"}`,
	`{"handle":"plan: 1"}`, `{"handle":"nope:1"}`, `{"handle":"PLAN:1"}`,
	`{"handle":"change:bad id"}`, `{"handle":"change:` + strings.Repeat("e", 129) + `"}`,
	`{"handle":"change_approval:99999999999999999999"}`, `{"handle":"plan:1","extra":1}`,
	`{"handle":"plan:1:2"}`, `{"handle":"agent_request:1\u0000"}`}

func TestRequestStatusMalformedHandleIsInvalidArguments(t *testing.T) {
	backend := newStatusBackend()
	server := waitServer(backend, newFakeHub())
	for _, args := range badHandleArguments {
		for _, tool := range []string{toolGetRequestStatus, toolWaitForDecision} {
			detail := mustFail(t, callTool(server, waitAgentCtx("a1"), tool, args),
				"invalid_arguments")
			require.Contains(t, detail["message"], "handle", "%s %s", tool, args)
			next := objectMap(t, detail["next"])
			require.Equal(t, tool, next["tool"], "%s %s: fix the call", tool, args)
		}
	}
	require.Equal(t, 0, backend.readCount(), "a malformed handle reads nothing")
}

func TestRequestStatusAcceptsEveryHandleKind(t *testing.T) {
	backend := newStatusBackend()
	handles := []string{"change_approval:1", "plan:9223372036854775807", "agent_request:2",
		"policy_exception:3", "change:ev-9", "change:0f3c_a.b:c"}
	for _, handle := range handles {
		backend.set(handle, ownedBy("a1", "pending", false))
		content := mustSucceed(t, callTool(waitServer(backend, nil), waitAgentCtx("a1"),
			toolGetRequestStatus, `{"handle":"`+handle+`"}`))
		require.Equal(t, handle, content["handle"])
		require.Equal(t, "pending", content["state"], handle)
		require.Equal(t, "", content["decided_by_kind"], "%s: nobody decided yet", handle)
	}
}

func TestRequestStatusUnknownHandleIsNotFound(t *testing.T) {
	backend := newStatusBackend()
	server := waitServer(backend, newFakeHub())
	for _, tool := range []string{toolGetRequestStatus, toolWaitForDecision} {
		detail := mustFail(t, callTool(server, waitAgentCtx("a1"), tool,
			`{"handle":"plan:404"}`), "not_found")
		require.Equal(t, "request plan:404 not found", detail["message"], tool)
		next := objectMap(t, detail["next"])
		require.Equal(t, tool, next["tool"], "%s: correct the handle", tool)
		require.Equal(t, []any{"handle"}, next["required"], tool)
	}
}

// CHECK-02: a principal cannot read another principal's handle; the
// answer is the same as for a handle that does not exist.
func TestRequestStatusOfAnotherPrincipalIsNotFound(t *testing.T) {
	backend := newStatusBackend()
	backend.set("agent_request:5", ownedBy("a1", "pending", false))
	backend.set("plan:6", RequestRecord{State: "pending", OwnerActor: "mcp:token:a1"})
	server := waitServer(backend, newFakeHub())
	callers := map[string]context.Context{
		"another agent": waitAgentCtx("b2"),
		"an admin person": WithPrincipal(context.Background(),
			Principal{Actor: "user:1", Role: "admin"}),
		"no principal": context.Background(),
		"same token id, other principal": WithPrincipal(context.Background(),
			Principal{Actor: "token:zz", Kind: KindAgent, PrincipalID: agpID("b2"),
				Scopes: []Scope{ScopeRead}}),
	}
	for who, ctx := range callers {
		for _, handle := range []string{"agent_request:5", "plan:6"} {
			for _, tool := range []string{toolGetRequestStatus, toolWaitForDecision} {
				got := callTool(server, ctx, tool, `{"handle":"`+handle+`"}`)
				detail := mustFail(t, got, "not_found")
				require.Equal(t, "request "+handle+" not found", detail["message"],
					"%s via %s", who, tool)
			}
		}
	}
	owner := mustSucceed(t, callTool(server, waitAgentCtx("a1"), toolGetRequestStatus,
		`{"handle":"plan:6"}`))
	require.Equal(t, "pending", owner["state"], "the proposer's actor owns the plan")
}

func TestRequestStatusStoreFailureIsUnavailableWithRetry(t *testing.T) {
	backend := newStatusBackend()
	backend.fail(errStoreDown)
	detail := mustFail(t, callTool(waitServer(backend, nil), waitAgentCtx("a1"),
		toolGetRequestStatus, `{"handle":"plan:1"}`), "unavailable")
	require.Contains(t, detail["message"], "plan:1")
	require.NotContains(t, detail["message"], "connection refused", "no store internals")
	require.Equal(t, "wait", detail["next"])
	require.Equal(t, float64(30), detail["retry_after"])
}

func TestRequestStatusWithoutABackendIsUnavailable(t *testing.T) {
	server := NewServer(&recordingBackend{}).WithDirectory(fleetOf("orders"))
	detail := mustFail(t, callTool(server, waitAgentCtx("a1"), toolGetRequestStatus,
		`{"handle":"plan:1"}`), "unavailable")
	require.Contains(t, detail["message"], "request status")
}

func TestRequestStatusNeedsADatabaseInAFleet(t *testing.T) {
	backend := newStatusBackend()
	server := NewServer(backend).WithDirectory(fleetOf("orders", "billing"))
	mustFail(t, callTool(server, waitAgentCtx("a1"), toolGetRequestStatus,
		`{"handle":"plan:1"}`), "database_required")
	backend.set("plan:1", ownedBy("a1", "pending", false))
	content := mustSucceed(t, callTool(server, waitAgentCtx("a1"), toolGetRequestStatus,
		`{"handle":"plan:1","database":"orders"}`))
	require.Equal(t, "orders", content["database"])
	mustFail(t, callTool(server, waitAgentCtx("a1"), toolGetRequestStatus,
		`{"handle":"plan:1","database":"billing"}`), "not_found")
}

func TestRequestStatusToolsAreReadScopeAndDescribed(t *testing.T) {
	for _, tool := range []string{toolGetRequestStatus, toolWaitForDecision} {
		scope, ok := RequiredScope(tool, nil)
		require.True(t, ok, tool)
		require.Equal(t, ScopeRead, scope, tool)
	}
	for _, tool := range NewServer(&recordingBackend{}).Tools() {
		if tool.Name != toolWaitForDecision {
			continue
		}
		schema := decodeSchema(t, tool.InputSchema)
		timeout := objectMap(t, objectMap(t, schema["properties"])["timeout_seconds"])
		require.Equal(t, json.Number("1"), timeout["minimum"])
		require.Equal(t, json.Number("60"), timeout["maximum"])
		return
	}
	t.Fatal("wait_for_decision is not listed")
}

// Held results now point at get_request_status with their handle, and
// following status reads the item (the loop mcp-01a left open).
func TestHeldResultStatusIsGetRequestStatus(t *testing.T) {
	backend := &recordingBackend{proposalResult: PolicyProposalResult{ProposalID: 42}}
	content := heldContent(t, changeServer(backend), operatorContext(context.Background()),
		"propose_policy_change", `{"delta":{"budgets":{"storage_bytes":0}}}`)
	status := objectMap(t, content["status"])
	require.Equal(t, toolGetRequestStatus, status["tool"])
	require.Equal(t, map[string]any{"handle": "policy_exception:42", "database": "orders"},
		status["arguments"])
	wait := objectMap(t, content["wait"])
	require.Equal(t, toolWaitForDecision, wait["tool"])
	require.Equal(t, status["arguments"], wait["arguments"])
}

func TestProposedPlanIsHeldWithAPlanHandle(t *testing.T) {
	backend := &planBackend{}
	server := NewServer(backend).WithDirectory(fleetOf("orders"))
	got := callTool(server, waitAgentCtx("a1"), "propose_plan",
		proposeArgs(planJSON(planStep)))
	content := mustSucceed(t, got)
	id := content["plan_id"]
	require.NotNil(t, id)
	handle := "plan:" + jsonText(id)
	require.Equal(t, handle, content["handle"])
	require.Equal(t, "#/actions?tab=plans&plan="+jsonText(id)+"&database=orders",
		content["approval_url"])
	require.Equal(t, "proposed", content["status"], "the plan's own status is kept")
	wait := objectMap(t, content["wait"])
	require.Equal(t, toolWaitForDecision, wait["tool"])
	require.Equal(t, map[string]any{"handle": handle, "database": "orders"},
		wait["arguments"])
}

func jsonText(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// canTrack points a held result at the status tools only for a caller
// ownsRequest can match: never an unbound caller, a person only where the
// store records the proposing actor (plans, policy proposals).
func TestCanTrackMatchesWhatOwnershipCanProve(t *testing.T) {
	person := operatorContext(context.Background())
	tokenAgent := agentContext(ScopeRead, ScopePropose) // an agent token, no principal id
	cases := []struct {
		name   string
		ctx    context.Context
		handle string
		want   bool
	}{
		{"unbound plan", context.Background(), "plan:1", false},
		{"blank actor plan", WithPrincipal(context.Background(), Principal{Actor: " "}),
			"plan:1", false},
		{"person plan", person, "plan:1", true},
		{"person policy", person, "policy_exception:2", true},
		{"person change", person, "change:ev-1", false},
		{"person queue item", person, "change_approval:3", false},
		{"person capability", person, "agent_request:4", false},
		{"token agent plan", tokenAgent, "plan:1", true},
		{"token agent queue item", tokenAgent, "change_approval:3", false},
		{"principal change", waitAgentCtx("trk"), "change:ev-1", true},
		{"principal queue item", waitAgentCtx("trk"), "change_approval:3", true},
		{"principal capability", waitAgentCtx("trk"), "agent_request:4", true},
	}
	for _, c := range cases {
		require.Equal(t, c.want, canTrack(c.ctx, c.handle), c.name)
	}
}
