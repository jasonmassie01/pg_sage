package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/earned"
)

// Sage SRE M7 autonomy tools (AI-SRE-SPEC §9): an external agent reads
// the earned-autonomy ledger (any role) and may step autonomy down
// (operator) when it sees trouble pg_sage does not. No tool approves a
// promotion: the approval is the human trust step and stays in the UI/API.

type autonomyToolBackend struct {
	mcpNoopBackend
	got   []AutonomyRequest
	actor string
	err   error
}

func (b *autonomyToolBackend) GetAutonomy(_ context.Context, r AutonomyRequest) (any, error) {
	b.got = append(b.got, r)
	return map[string]any{"database": r.Database, "families": []any{}}, b.err
}

func (b *autonomyToolBackend) DowngradeAutonomy(_ context.Context, r AutonomyRequest,
	actor string) (any, error) {
	b.got = append(b.got, r)
	b.actor = actor
	return map[string]any{"states": []any{}}, b.err
}

var autonomyOperatorCtx = WithPrincipal(context.Background(),
	Principal{Actor: "user:2", Role: "operator"})

const downgradeArgs = `{"database":"orders","family":"wal_retention",` +
	`"action_class":"wal_bound","level":"L1","reason":"replica lag climbing"}`

func TestAutonomyTools_ListedStrictAndWithoutApproval(t *testing.T) {
	s := NewServer(&autonomyToolBackend{})
	found := map[string]bool{}
	for _, tool := range s.Tools() {
		found[tool.Name] = true
		if strings.Contains(tool.Name, "approve") || strings.Contains(tool.Name, "promot") {
			t.Fatalf("MCP exposes %s: promotions need a human approver", tool.Name)
		}
		if strings.HasPrefix(tool.Name, "sre_") && strings.Contains(tool.Name, "autonomy") &&
			!strings.Contains(string(tool.InputSchema), `"additionalProperties":false`) {
			t.Errorf("%s schema is not strict", tool.Name)
		}
	}
	if !found["sre_get_autonomy"] || !found["sre_downgrade_autonomy"] {
		t.Fatalf("tools = %v", found)
	}
}

func TestAutonomyTools_ViewerReads(t *testing.T) {
	b := &autonomyToolBackend{}
	result, rpcErr := callSRETool(t, NewServer(b), viewerCtx, "sre_get_autonomy",
		`{"database":"orders"}`)
	if rpcErr != nil || len(b.got) != 1 || b.got[0].Database != "orders" ||
		result["structuredContent"] == nil {
		t.Fatalf("get = %v %v (%+v)", result, rpcErr, b.got)
	}
}

func TestAutonomyTools_DowngradeNeedsAnOperator(t *testing.T) {
	b := &autonomyToolBackend{}
	if _, rpcErr := callSRETool(t, NewServer(b), viewerCtx, "sre_downgrade_autonomy",
		downgradeArgs); rpcErr == nil || rpcErr["code"] != float64(-32001) || len(b.got) != 0 {
		t.Fatalf("viewer downgrade = %v (%+v)", rpcErr, b.got)
	}
	if _, rpcErr := callSRETool(t, NewServer(b), context.Background(),
		"sre_downgrade_autonomy", downgradeArgs); rpcErr == nil || len(b.got) != 0 {
		t.Fatalf("unbound downgrade = %v", rpcErr)
	}
	result, rpcErr := callSRETool(t, NewServer(b), autonomyOperatorCtx, "sre_downgrade_autonomy",
		downgradeArgs)
	if rpcErr != nil || result == nil || len(b.got) != 1 || b.actor != "mcp:user:2" {
		t.Fatalf("operator downgrade = %v %v actor=%q", result, rpcErr, b.actor)
	}
	r := b.got[0]
	if r.Family != "wal_retention" || r.ActionClass != "wal_bound" || r.Level != "L1" ||
		r.Reason != "replica lag climbing" {
		t.Fatalf("request = %+v", r)
	}
}

func TestAutonomyTools_RejectBadArguments(t *testing.T) {
	b := &autonomyToolBackend{}
	s := NewServer(b)
	for name, args := range map[string]string{
		"unknown field": `{"database":"orders","sql":"DROP TABLE x"}`,
		"no family":     `{"action_class":"wal_bound","level":"L1","reason":"r"}`,
		"no level":      `{"family":"wal_retention","action_class":"wal_bound","reason":"r"}`,
		"no reason":     `{"family":"wal_retention","action_class":"wal_bound","level":"L1"}`,
		"not an object": `"downgrade everything"`,
	} {
		if _, rpcErr := callSRETool(t, s, autonomyOperatorCtx, "sre_downgrade_autonomy",
			args); rpcErr == nil || rpcErr["code"] != float64(-32602) {
			t.Errorf("%s: %v", name, rpcErr)
		}
	}
	if len(b.got) != 0 {
		t.Fatalf("bad arguments reached the backend: %+v", b.got)
	}
}

func TestAutonomyTools_MapLedgerErrors(t *testing.T) {
	for err, code := range map[error]float64{
		earned.ErrInvalidRequest: -32602, earned.ErrNotADowngrade: -32602,
		earned.ErrUnavailable: -32603, context.Canceled: -32800,
		errors.New("boom"): -32603,
	} {
		b := &autonomyToolBackend{err: err}
		if _, rpcErr := callSRETool(t, NewServer(b), autonomyOperatorCtx, "sre_downgrade_autonomy",
			downgradeArgs); rpcErr == nil || rpcErr["code"] != code {
			t.Errorf("%v -> %v, want %v", err, rpcErr, code)
		}
	}
	if _, rpcErr := callSRETool(t, NewServer(mcpNoopBackend{}), viewerCtx, "sre_get_autonomy",
		`{}`); rpcErr == nil || rpcErr["code"] != float64(-32603) {
		t.Fatalf("backend without autonomy: %v", rpcErr)
	}
}
