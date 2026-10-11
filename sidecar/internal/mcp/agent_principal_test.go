package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// spec §6.2.6 (CG-08): every ActionRequest built from an agent's
// MCP call carries the agent. The server binds a policy.PrincipalRef with
// the tool name on the call's context; the gate fills Principal from it.

// refBackend records the principal ref each backend call sees.
type refBackend struct {
	intentRecordingBackend
	mu   sync.Mutex
	refs []*policy.PrincipalRef
}

func (b *refBackend) see(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ref, ok := policy.PrincipalRefFromContext(ctx); ok {
		b.refs = append(b.refs, &ref)
		return
	}
	b.refs = append(b.refs, nil)
}

func (b *refBackend) RequestIntent(ctx context.Context, tool string,
	args json.RawMessage) (any, error) {
	b.see(ctx)
	return b.intentRecordingBackend.RequestIntent(ctx, tool, args)
}

func (b *refBackend) RequestChange(ctx context.Context,
	request ChangeRequest) (ChangeResult, error) {
	b.see(ctx)
	return b.intentRecordingBackend.RequestChange(ctx, request)
}

func (b *refBackend) ProposePolicyChange(ctx context.Context,
	request PolicyProposalRequest) (PolicyProposalResult, error) {
	b.see(ctx)
	return b.intentRecordingBackend.ProposePolicyChange(ctx, request)
}

func agentPrincipal(id string) Principal {
	return Principal{Actor: "token:t1", Kind: KindAgent, PrincipalID: id,
		Scopes: []Scope{ScopeRead, ScopePropose}, TokenID: "t1"}
}

// agentProposeCalls are the mutating tools an agent may call (propose
// scope); the approve-scope ones are refused before any backend.
func agentProposeCalls() map[string]string {
	calls := map[string]string{}
	for name, args := range mutatingToolCalls {
		if !approveScopeTools[name] {
			calls[name] = args
		}
	}
	return calls
}

func TestAgentCallsCarryPrincipalAndTool(t *testing.T) {
	ctx := WithPrincipal(context.Background(), agentPrincipal("agp_aaaaaaaaaaaaaaaaaaaa"))
	for name, args := range agentProposeCalls() {
		backend := &refBackend{}
		response := invoke(t, NewServer(backend), ctx, toolCall(name, args))
		require.Empty(t, response.Error.Code, name)
		require.Equal(t, 1, len(backend.refs), name)
		ref := backend.refs[0]
		if ref == nil || ref.ID != "agp_aaaaaaaaaaaaaaaaaaaa" || ref.Tool != name {
			t.Fatalf("%s: ref = %+v, want the agent and the tool", name, ref)
		}
	}
}

func TestUnboundAgentCallsAreStillAgentOriginated(t *testing.T) {
	// The stdio client without mcp.stdio_principal.
	ctx := WithPrincipal(context.Background(), stdioPrincipal)
	backend := &refBackend{}
	response := invoke(t, NewServer(backend), ctx,
		toolCall("optimize_query", mutatingToolCalls["optimize_query"]))
	require.Empty(t, response.Error.Code)
	ref := backend.refs[0]
	if ref == nil || ref.ID != "" || ref.Tool != "optimize_query" {
		t.Fatalf("ref = %+v, want an unbound agent ref", ref)
	}
}

func TestHumanCallsCarryNoPrincipal(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Actor: "user:2", Role: "operator"})
	for name, args := range mutatingToolCalls {
		backend := &refBackend{}
		invoke(t, NewServer(backend), ctx, toolCall(name, args))
		for _, ref := range backend.refs {
			if ref != nil {
				t.Fatalf("%s: a person's call carries agent ref %+v", name, ref)
			}
		}
	}
}

func TestBoundRefKeepsSponsorAndTaskAndGainsTool(t *testing.T) {
	ctx := WithPrincipal(context.Background(), agentPrincipal("agp_aaaaaaaaaaaaaaaaaaaa"))
	ctx = policy.WithPrincipalRef(ctx, policy.PrincipalRef{ID: "agp_aaaaaaaaaaaaaaaaaaaa",
		SponsorID: 7, TaskID: "task-3", OnBehalfOf: "alice", Tool: "stale"})
	backend := &refBackend{}
	invoke(t, NewServer(backend), ctx, toolCall("request_change",
		mutatingToolCalls["request_change"]))
	ref := backend.refs[0]
	if ref == nil || ref.SponsorID != 7 || ref.TaskID != "task-3" ||
		ref.OnBehalfOf != "alice" || ref.Tool != "request_change" {
		t.Fatalf("ref = %+v, want the bound identity with this call's tool", ref)
	}
}

func TestToolArgumentsCannotSetPrincipal(t *testing.T) {
	ctx := WithPrincipal(context.Background(), agentPrincipal("agp_aaaaaaaaaaaaaaaaaaaa"))
	backend := &refBackend{}
	invoke(t, NewServer(backend), ctx, toolCall("optimize_query",
		`{"goal":"latency","query_id":1,"caller_claims":{"principal_id":"agp_x"}}`))
	if ref := backend.refs[0]; ref == nil || ref.ID != "agp_aaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("ref = %+v, want the authenticated agent only", ref)
	}
}

func TestAgentApproveToolsStayReserved(t *testing.T) {
	ctx := WithPrincipal(context.Background(), agentPrincipal("agp_aaaaaaaaaaaaaaaaaaaa"))
	for name := range approveScopeTools {
		backend := &refBackend{}
		args, ok := mutatingToolCalls[name]
		if !ok {
			args = `{}`
		}
		response := invoke(t, NewServer(backend), ctx, toolCall(name, args))
		require.Equal(t, codeApprovalReserved, response.Error.Code, name)
		require.Equal(t, 0, len(backend.refs), name)
	}
}

func TestStdioPrincipalBinding(t *testing.T) {
	bound := StdioPrincipalFor("agp_bbbbbbbbbbbbbbbbbbbb")
	if bound.PrincipalID != "agp_bbbbbbbbbbbbbbbbbbbb" || bound.Kind != KindAgent ||
		bound.Has(ScopeApprove) || !bound.Has(ScopePropose) || bound.Actor == "" {
		t.Fatalf("bound stdio principal = %+v", bound)
	}
	unbound := StdioPrincipalFor("")
	if unbound.PrincipalID != "" || unbound.Actor != stdioPrincipal.Actor {
		t.Fatalf("unbound stdio principal = %+v, want today's", unbound)
	}
}
