package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Sage SRE read tools (AI-SRE-SPEC §9): an external agent lists
// investigations and reads one investigation or one evidence item. They
// are read-only (viewer), take typed arguments only, and never start,
// stop or execute anything.

type sreToolBackend struct {
	mcpNoopBackend
	last InvestigationRequest
	err  error
}

func (b *sreToolBackend) ListInvestigations(_ context.Context,
	r InvestigationRequest) (any, error) {
	b.last = r
	return map[string]any{"items": []any{}}, b.err
}

func (b *sreToolBackend) GetInvestigation(_ context.Context,
	r InvestigationRequest) (any, error) {
	b.last = r
	return map[string]any{"id": r.InvestigationID}, b.err
}

func (b *sreToolBackend) GetEvidence(_ context.Context, r InvestigationRequest) (any, error) {
	b.last = r
	return map[string]any{"id": r.EvidenceID}, b.err
}

// mcpNoopBackend satisfies Backend for the SRE tool tests.
type mcpNoopBackend struct{}

func (mcpNoopBackend) GetPolicy(context.Context, PolicyRequest) (PolicyResult, error) {
	return PolicyResult{}, nil
}
func (mcpNoopBackend) ProposePolicyChange(context.Context,
	PolicyProposalRequest) (PolicyProposalResult, error) {
	return PolicyProposalResult{}, nil
}
func (mcpNoopBackend) RequestChange(context.Context, ChangeRequest) (ChangeResult, error) {
	return ChangeResult{}, nil
}
func (mcpNoopBackend) GetLedger(context.Context, LedgerRequest) (LedgerResult, error) {
	return LedgerResult{}, nil
}

func callSRETool(t *testing.T, s *Server, ctx context.Context, name, args string) (
	map[string]any, map[string]any) {
	t.Helper()
	raw := s.Handle(ctx, json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,`+
		`"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args)))
	var resp struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("response: %v (%s)", err, raw)
	}
	if resp.Result["isError"] == true { // a tool execution error (MCP 2025-06-18+)
		structured, _ := resp.Result["structuredContent"].(map[string]any)
		failure, _ := structured["error"].(map[string]any)
		return nil, failure
	}
	return resp.Result, resp.Error
}

var viewerCtx = WithPrincipal(context.Background(), Principal{Actor: "user:3", Role: "viewer"})

func TestSRETools_ListedAsReadOnlyWithStrictSchemas(t *testing.T) {
	s := NewServer(&sreToolBackend{})
	found := map[string]Tool{}
	for _, tool := range s.Tools() {
		found[tool.Name] = tool
	}
	for _, name := range []string{"sre_list_incidents", "sre_get_investigation",
		"sre_get_evidence"} {
		tool, ok := found[name]
		if !ok {
			t.Fatalf("tools/list lacks %s", name)
		}
		var schema map[string]any
		if json.Unmarshal(tool.InputSchema, &schema) != nil ||
			schema["additionalProperties"] != false {
			t.Errorf("%s schema is not strict: %s", name, tool.InputSchema)
		}
		if mutatingTools[name] {
			t.Errorf("%s is marked mutating; it is read-only", name)
		}
	}
}

func TestSRETools_ViewerReadsWithTypedArguments(t *testing.T) {
	b := &sreToolBackend{}
	s := NewServer(b)
	res, rpcErr := callSRETool(t, s, viewerCtx, "sre_get_investigation",
		`{"database":"orders","investigation_id":"11111111-1111-4111-8111-111111111111"}`)
	if rpcErr != nil || res == nil || b.last.Database != "orders" ||
		b.last.InvestigationID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("get_investigation = %v %v, backend saw %+v", res, rpcErr, b.last)
	}
	if _, rpcErr := callSRETool(t, s, viewerCtx, "sre_get_evidence",
		`{"investigation_id":"a","evidence_id":"b"}`); rpcErr != nil ||
		b.last.EvidenceID != "b" {
		t.Fatalf("get_evidence = %v, backend saw %+v", rpcErr, b.last)
	}
	if _, rpcErr := callSRETool(t, s, viewerCtx, "sre_list_incidents",
		`{"case_id":"incident:orders:lock:1"}`); rpcErr != nil ||
		b.last.CaseID != "incident:orders:lock:1" {
		t.Fatalf("list = %v, backend saw %+v", rpcErr, b.last)
	}
}

func TestSRETools_RejectInvalidArguments(t *testing.T) {
	s := NewServer(&sreToolBackend{})
	for name, args := range map[string]string{
		"sre_get_investigation": `{}`,
		"sre_get_evidence":      `{"investigation_id":"a"}`,
		"sre_list_incidents":    `{"sql":"DROP TABLE x"}`,
	} {
		if _, rpcErr := callSRETool(t, s, viewerCtx, name, args); rpcErr == nil ||
			rpcErr["code"] != float64(-32602) {
			t.Errorf("%s %s = %v, want -32602", name, args, rpcErr)
		}
	}
}

// Error propagation: not found and unavailable are distinguishable.
func TestSRETools_ErrorsAreDistinguishable(t *testing.T) {
	cases := map[error]float64{sre.ErrNotFound: -32004, sre.ErrInvalidRequest: -32602,
		sre.ErrMetadataUnavailable: -32603, errors.New("boom"): -32603}
	for err, code := range cases {
		s := NewServer(&sreToolBackend{err: err})
		_, rpcErr := callSRETool(t, s, viewerCtx, "sre_get_investigation",
			`{"investigation_id":"x"}`)
		if rpcErr == nil || rpcErr["code"] != code {
			t.Errorf("%v -> %v, want code %v", err, rpcErr, code)
		}
	}
}

// A backend without investigations reports the tool as unavailable.
func TestSRETools_BackendWithoutInvestigations(t *testing.T) {
	s := NewServer(mcpNoopBackend{})
	if _, rpcErr := callSRETool(t, s, viewerCtx, "sre_list_incidents", `{}`); rpcErr == nil {
		t.Fatal("a backend without investigations answered")
	}
	backend, err := NewProductionBackend(ProductionDependencies{})
	if err == nil || backend != nil {
		t.Fatalf("empty production dependencies accepted: %v", err)
	}
}

// The production backend serves the tools from its investigations
// dependency and reports it unavailable without one.
func TestSRETools_ProductionBackendDelegates(t *testing.T) {
	inv := &sreToolBackend{}
	b := &ProductionBackend{dependencies: ProductionDependencies{Investigations: inv}}
	if _, err := b.GetInvestigation(context.Background(),
		InvestigationRequest{InvestigationID: "x"}); err != nil ||
		inv.last.InvestigationID != "x" {
		t.Fatalf("delegation: %v %+v", err, inv.last)
	}
	empty := &ProductionBackend{}
	for _, call := range []func() (any, error){
		func() (any, error) {
			return empty.ListInvestigations(context.Background(), InvestigationRequest{})
		},
		func() (any, error) {
			return empty.GetEvidence(context.Background(), InvestigationRequest{})
		},
	} {
		if _, err := call(); !errors.Is(err, ErrProductionDependencyUnavailable) {
			t.Fatalf("without investigations = %v", err)
		}
	}
}
