package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Sage SRE M5 read tools: an external agent reads SLO error-budget state
// (with the SLI recovery predicate) and the change feed. They are
// read-only, typed and never execute anything.

type signalToolBackend struct {
	mcpNoopBackend
	last SignalRequest
	tool string
	err  error
}

func (b *signalToolBackend) ListSLOs(_ context.Context, r SignalRequest) (any, error) {
	b.last, b.tool = r, "list_slos"
	return map[string]any{"slos": []any{}}, b.err
}

func (b *signalToolBackend) GetSLO(_ context.Context, r SignalRequest) (any, error) {
	b.last, b.tool = r, "get_slo"
	return map[string]any{"name": r.Name}, b.err
}

func (b *signalToolBackend) ListChanges(_ context.Context, r SignalRequest) (any, error) {
	b.last, b.tool = r, "list_changes"
	return map[string]any{"changes": []any{}}, b.err
}

func TestSignalTools_Listed(t *testing.T) {
	s := NewServer(&signalToolBackend{})
	names := map[string]bool{}
	for _, tool := range s.Tools() {
		names[tool.Name] = true
	}
	for _, want := range []string{"sre_list_slos", "sre_get_slo", "sre_list_changes",
		"sre_list_incidents"} {
		if !names[want] {
			t.Errorf("tool %s not listed", want)
		}
	}
	for _, mutating := range []string{"sre_list_slos", "sre_get_slo", "sre_list_changes"} {
		if scope, _ := RequiredScope(mutating, nil); scope != ScopeRead {
			t.Errorf("%s is marked mutating", mutating)
		}
	}
}

func TestSignalTools_Calls(t *testing.T) {
	b := &signalToolBackend{}
	s := NewServer(b)
	ctx := context.Background()
	res, rpcErr := callSRETool(t, s, ctx, "sre_list_slos", `{"database":"orders"}`)
	if rpcErr != nil || res == nil || b.tool != "list_slos" || b.last.Database != "orders" {
		t.Fatalf("list: %v %v %+v", res, rpcErr, b.last)
	}
	_, rpcErr = callSRETool(t, s, ctx, "sre_get_slo",
		`{"name":"checkout","recovery_since":"2026-10-01T11:00:00Z"}`)
	if rpcErr != nil || b.tool != "get_slo" || b.last.Name != "checkout" ||
		b.last.RecoverySince != "2026-10-01T11:00:00Z" {
		t.Fatalf("get: %v %+v", rpcErr, b.last)
	}
	_, rpcErr = callSRETool(t, s, ctx, "sre_list_changes", `{"window_minutes":120}`)
	if rpcErr != nil || b.tool != "list_changes" || b.last.WindowMinutes != 120 {
		t.Fatalf("changes: %v %+v", rpcErr, b.last)
	}
	_, rpcErr = callSRETool(t, s, ctx, "sre_list_changes", `{}`)
	if rpcErr != nil || b.last.WindowMinutes != 60 {
		t.Fatalf("default window: %v %+v", rpcErr, b.last)
	}
}

func TestSignalTools_InvalidArguments(t *testing.T) {
	s := NewServer(&signalToolBackend{})
	ctx := context.Background()
	for name, c := range map[string]struct{ tool, args string }{
		"get without name":  {"sre_get_slo", `{}`},
		"unknown argument":  {"sre_list_slos", `{"sql":"select 1"}`},
		"bad since":         {"sre_get_slo", `{"name":"x","recovery_since":"yesterday"}`},
		"window too long":   {"sre_list_changes", `{"window_minutes":10081}`},
		"negative window":   {"sre_list_changes", `{"window_minutes":-1}`},
		"wrong type":        {"sre_list_changes", `{"window_minutes":"60"}`},
		"name with control": {"sre_get_slo", "{\"name\":\"a\\nb\"}"},
	} {
		if _, rpcErr := callSRETool(t, s, ctx, c.tool, c.args); rpcErr == nil ||
			rpcErr["code"] != float64(-32602) {
			t.Errorf("%s: error = %v, want invalid arguments", name, rpcErr)
		}
	}
}

func TestSignalTools_ErrorsAndMissingBackend(t *testing.T) {
	b := &signalToolBackend{err: sre.ErrNotFound}
	s := NewServer(b)
	ctx := context.Background()
	if _, rpcErr := callSRETool(t, s, ctx, "sre_get_slo", `{"name":"x"}`); rpcErr == nil ||
		rpcErr["code"] != float64(-32004) {
		t.Fatalf("not found = %v", rpcErr)
	}
	b.err = errors.New("boom: secret detail")
	_, rpcErr := callSRETool(t, s, ctx, "sre_list_slos", `{}`)
	if rpcErr == nil || rpcErr["message"] != "internal error" {
		t.Fatalf("internal = %v", rpcErr)
	}
	plain := NewServer(mcpNoopBackend{})
	if _, rpcErr := callSRETool(t, plain, ctx, "sre_list_slos", `{}`); rpcErr == nil ||
		rpcErr["code"] != float64(-32603) {
		t.Fatalf("no signal backend = %v", rpcErr)
	}
}
