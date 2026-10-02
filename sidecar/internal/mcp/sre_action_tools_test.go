package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// Sage SRE action tools (AI-SRE-SPEC §9, CHECK-39): sre_propose_action
// returns a typed proposal with its repair contract and policy verdict;
// sre_request_execution queues exactly one item in the existing approval
// flow. Both need an operator or admin principal, take typed ids only,
// and never execute anything.

type sreActionBackend struct {
	sreToolBackend
	proposeCalls, requestCalls int
	last                       SREActionRequest
	actor                      string
	err                        error
}

func (b *sreActionBackend) ProposeAction(ctx context.Context,
	r SREActionRequest) (any, error) {
	b.proposeCalls++
	b.last, b.actor = r, ActorFromContext(ctx)
	return map[string]any{"id": "p-1", "state": "proposed"}, b.err
}

func (b *sreActionBackend) RequestExecution(ctx context.Context,
	r SREActionRequest) (any, error) {
	b.requestCalls++
	b.last, b.actor = r, ActorFromContext(ctx)
	return map[string]any{"id": r.ProposalID, "queue_id": 9}, b.err
}

var operatorCtx = WithPrincipal(context.Background(),
	Principal{Actor: "user:2", Role: "operator"})

const (
	invArgs      = `{"investigation_id":"11111111-1111-4111-8111-111111111111"}`
	proposalArgs = `{"database":"orders","proposal_id":"22222222-2222-4222-8222-222222222222"}`
)

func TestSREActionTools_ListedWithStrictSchemas(t *testing.T) {
	found := map[string]Tool{}
	for _, tool := range NewServer(&sreActionBackend{}).Tools() {
		found[tool.Name] = tool
	}
	for name, required := range map[string]string{
		"sre_propose_action": "investigation_id", "sre_request_execution": "proposal_id",
	} {
		tool, ok := found[name]
		if !ok {
			t.Fatalf("%s not listed", name)
		}
		var schema struct {
			Required             []string `json:"required"`
			AdditionalProperties bool     `json:"additionalProperties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		if len(schema.Required) != 1 || schema.Required[0] != required ||
			schema.AdditionalProperties {
			t.Fatalf("%s schema = %s", name, tool.InputSchema)
		}
		if !strings.Contains(strings.ToLower(tool.Description), "never execut") {
			t.Fatalf("%s description does not say it never executes: %q", name,
				tool.Description)
		}
	}
}

func TestSREActionTools_RequireAnOperator(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no principal": context.Background(),
		"viewer":       viewerCtx,
		"blank actor": WithPrincipal(context.Background(),
			Principal{Actor: " ", Role: "admin"}),
	} {
		b := &sreActionBackend{}
		s := NewServer(b)
		for tool, args := range map[string]string{"sre_propose_action": invArgs,
			"sre_request_execution": proposalArgs} {
			_, rpcErr := callSRETool(t, s, ctx, tool, args)
			if rpcErr == nil || rpcErr["code"] != float64(-32001) {
				t.Errorf("%s calling %s = %v, want -32001", name, tool, rpcErr)
			}
		}
		if b.proposeCalls+b.requestCalls != 0 {
			t.Fatalf("%s reached the backend", name)
		}
	}
}

func TestSREActionTools_OperatorCallsArePrincipalBound(t *testing.T) {
	b := &sreActionBackend{}
	s := NewServer(b)
	res, rpcErr := callSRETool(t, s, operatorCtx, "sre_request_execution", proposalArgs)
	if rpcErr != nil || b.requestCalls != 1 || b.proposeCalls != 0 {
		t.Fatalf("request_execution = %v / %v, calls %d/%d", res, rpcErr,
			b.requestCalls, b.proposeCalls)
	}
	if b.last.Database != "orders" || b.last.ProposalID !=
		"22222222-2222-4222-8222-222222222222" || b.actor != "mcp:user:2" {
		t.Fatalf("backend saw %+v as %q", b.last, b.actor)
	}
	content, _ := res["structuredContent"].(map[string]any)
	if content["queue_id"] != float64(9) {
		t.Fatalf("result = %v", res)
	}
	admin := WithPrincipal(context.Background(), Principal{Actor: "user:1", Role: "admin"})
	if _, rpcErr := callSRETool(t, s, admin, "sre_propose_action", invArgs); rpcErr != nil ||
		b.proposeCalls != 1 || b.last.InvestigationID == "" {
		t.Fatalf("admin propose = %v, calls %d", rpcErr, b.proposeCalls)
	}
}

func TestSREActionTools_TypedArgumentsOnly(t *testing.T) {
	b := &sreActionBackend{}
	s := NewServer(b)
	for tool, args := range map[string]string{
		"sre_propose_action":    `{"investigation_id":"x","sql":"SELECT pg_terminate_backend(1)"}`,
		"sre_request_execution": `{}`,
	} {
		if _, rpcErr := callSRETool(t, s, operatorCtx, tool, args); rpcErr == nil ||
			rpcErr["code"] != float64(-32602) {
			t.Errorf("%s %s = %v, want -32602", tool, args, rpcErr)
		}
	}
	if _, rpcErr := callSRETool(t, s, operatorCtx, "sre_propose_action", `{}`); rpcErr == nil {
		t.Error("propose without an investigation id accepted")
	}
	if b.proposeCalls+b.requestCalls != 0 {
		t.Fatal("invalid arguments reached the backend")
	}
}

func TestSREActionTools_ErrorsAreDistinguishable(t *testing.T) {
	for err, code := range map[error]float64{
		sreaction.ErrProposalNotFound: -32004,
		sreaction.ErrProposalState:    -32009,
		sreaction.ErrPolicyBlocked:    -32010,
		sreaction.ErrHandoffBlocked:   -32603,
		sre.ErrInvalidRequest:         -32602,
		errors.New("boom"):            -32603,
	} {
		b := &sreActionBackend{err: err}
		_, rpcErr := callSRETool(t, NewServer(b), operatorCtx, "sre_request_execution",
			proposalArgs)
		if rpcErr == nil || rpcErr["code"] != code {
			t.Errorf("%v = %v, want code %v", err, rpcErr, code)
		}
	}
	_, rpcErr := callSRETool(t, NewServer(&sreToolBackend{}), operatorCtx,
		"sre_request_execution", proposalArgs)
	if rpcErr == nil || rpcErr["code"] != float64(-32603) {
		t.Fatalf("backend without action support = %v, want -32603", rpcErr)
	}
}
