package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// agent_request_capability (spec §8.2): an agent asks for a time-boxed
// grant; governance decides it and an operator approves it (L2). It needs
// the propose scope and an agent principal; a blocked request is a result
// with the reason and fix (§8.1), a rate limit is -32011 with retry_after.

type grantToolBackend struct {
	recordingBackend
	calls  int
	pid    string
	in     CapabilityRequest
	result CapabilityResult
	err    error
}

func (b *grantToolBackend) RequestCapability(_ context.Context, pid string,
	in CapabilityRequest) (CapabilityResult, error) {
	b.calls++
	b.pid, b.in = pid, in
	return b.result, b.err
}

const capabilityArgs = `{"database":"orders","capability":"read",` +
	`"objects":["app.orders"],"columns":{"app.orders":["id","total"]},` +
	`"duration_minutes":60,"reason":"weekly revenue report"}`

func grantAgentCtx() context.Context {
	return WithPrincipal(context.Background(), Principal{Actor: "token:t1", Kind: KindAgent,
		Scopes: []Scope{ScopeRead, ScopePropose}, PrincipalID: "agp_aaaaaaaaaaaaaaaaaaaa"})
}

func TestRequestCapability_IsListedAsAProposeTool(t *testing.T) {
	var found *Tool
	for _, tool := range NewServer(&grantToolBackend{}).Tools() {
		if tool.Name == "agent_request_capability" {
			tool := tool
			found = &tool
		}
	}
	require.NotNil(t, found)
	require.False(t, found.Annotations.ReadOnlyHint)
	scope, ok := RequiredScope("agent_request_capability", nil)
	require.True(t, ok)
	require.Equal(t, ScopePropose, scope)
}

func TestRequestCapability_QueuesAndPassesTheRequest(t *testing.T) {
	exp := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	b := &grantToolBackend{result: CapabilityResult{
		Verdict: VerdictQueueApproval, ReasonCode: "approval_required", RequestID: 4,
		ExpiresAt: &exp, ApprovalURL: "/agents/x"}}
	response := invoke(t, NewServer(b), grantAgentCtx(), toolCall("agent_request_capability",
		capabilityArgs))
	require.Empty(t, response.Error.Code)
	require.Equal(t, 1, b.calls)
	require.Equal(t, "agp_aaaaaaaaaaaaaaaaaaaa", b.pid)
	require.Equal(t, CapabilityRequest{Database: "orders", Capability: "read",
		Objects: []CapabilityObject{{Object: "app.orders", Columns: []string{"id",
			"total"}}}, DurationMinutes: 60, Reason: "weekly revenue report"}, b.in)
	out := structuredContent(t, response)
	require.Equal(t, "queue_approval", out["verdict"])
	require.Equal(t, "/agents/x", out["approval_url"])
}

func TestRequestCapability_BlockedIsAResultWithTheFix(t *testing.T) {
	b := &grantToolBackend{result: CapabilityResult{Verdict: VerdictBlocked,
		ReasonCode: "agent_capability", Fix: "add read to the profile"}}
	response := invoke(t, NewServer(b), grantAgentCtx(), toolCall("agent_request_capability",
		capabilityArgs))
	require.Empty(t, response.Error.Code)
	out := structuredContent(t, response)
	require.Equal(t, "blocked", out["verdict"])
	require.Equal(t, "add read to the profile", out["fix"])
}

func TestRequestCapability_ParkIsRateLimited(t *testing.T) {
	b := &grantToolBackend{result: CapabilityResult{Verdict: VerdictPark,
		ReasonCode: "agent_rate", RetryAfterSeconds: 30}}
	response := invoke(t, NewServer(b), grantAgentCtx(), toolCall("agent_request_capability",
		capabilityArgs))
	require.Equal(t, -32011, response.Error.Code)
	require.Contains(t, response.Error.Message, "30")
}

func TestRequestCapability_RefusesWithoutAnAgentPrincipal(t *testing.T) {
	cases := map[string]context.Context{
		"viewer": WithPrincipal(context.Background(), Principal{Actor: "u:3", Role: "viewer"}),
		"person": WithPrincipal(context.Background(), Principal{Actor: "u:2",
			Role: "operator"}),
	}
	want := map[string]int{"viewer": -32001, "person": -32003}
	for name, ctx := range cases {
		b := &grantToolBackend{}
		response := invoke(t, NewServer(b), ctx, toolCall("agent_request_capability",
			capabilityArgs))
		require.Equal(t, want[name], response.Error.Code, name)
		require.Equal(t, 0, b.calls, name)
	}
}

// A legacy agent token (no principal) reaches governance with no principal
// id, which decides it as unsponsored (G1-11); it is never refused here.
func TestRequestCapability_LegacyTokenGoesToGovernance(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Actor: "token:x", Kind: KindAgent,
		Scopes: []Scope{ScopeRead, ScopePropose}})
	b := &grantToolBackend{result: CapabilityResult{Verdict: VerdictBlocked,
		ReasonCode: "agent_unsponsored"}}
	response := invoke(t, NewServer(b), ctx, toolCall("agent_request_capability",
		capabilityArgs))
	require.Empty(t, response.Error.Code)
	require.Equal(t, 1, b.calls)
	require.Equal(t, "", b.pid)
	require.Equal(t, "agent_unsponsored", structuredContent(t, response)["reason_code"])
}

func TestRequestCapability_ValidatesArguments(t *testing.T) {
	for name, args := range map[string]string{
		"no objects": `{"capability":"read","objects":[],"duration_minutes":5,"reason":"x"}`,
		"no reason":  `{"capability":"read","objects":["a.b"],"duration_minutes":5}`,
		"zero":       `{"capability":"read","objects":["a.b"],"duration_minutes":0,"reason":"x"}`,
		"unknown":    `{"capability":"read","objects":["a.b"],"duration_minutes":5,"reason":"x","x":1}`,
		"stray cols": `{"capability":"read","objects":["a.b"],"columns":{"c.d":["e"]},` +
			`"duration_minutes":5,"reason":"x"}`,
		"capability": `{"objects":["a.b"],"duration_minutes":5,"reason":"x"}`,
	} {
		b := &grantToolBackend{}
		response := invoke(t, NewServer(b), grantAgentCtx(), toolCall("agent_request_capability",
			args))
		require.Equal(t, -32602, response.Error.Code, name)
		require.Equal(t, 0, b.calls, name)
	}
}

func TestRequestCapability_MapsErrors(t *testing.T) {
	cases := map[error]int{
		fmt.Errorf("x: %w", agentguard.ErrInvalid):     -32602,
		fmt.Errorf("x: %w", agentguard.ErrUnavailable): -32010,
		envbind.ErrUnknownDatabase:                     -32007,
		fmt.Errorf("x: %w", agentguard.ErrNotFound):    -32004,
		errors.New("boom"):                             -32603,
	}
	for err, code := range cases {
		b := &grantToolBackend{err: err}
		response := invoke(t, NewServer(b), grantAgentCtx(), toolCall("agent_request_capability",
			capabilityArgs))
		require.Equal(t, code, response.Error.Code, err.Error())
	}
	plain := invoke(t, NewServer(&recordingBackend{}), grantAgentCtx(),
		toolCall("agent_request_capability", capabilityArgs))
	require.Equal(t, -32010, plain.Error.Code, "no grant backend: unavailable")
}
