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

// playbooks-06: statistics maintenance over MCP. The server resolves the
// cited evidence itself, so no call carries an evidence id or SHA;
// refresh, propose and request need propose scope, rollback needs
// approve (agents never hold it).

type sreMaintenanceBackend struct {
	sreToolBackend
	calls map[string]int
	last  SREMaintenanceRequest
	actor string
	err   error
}

func (b *sreMaintenanceBackend) record(ctx context.Context, tool string,
	r SREMaintenanceRequest) (any, error) {
	if b.calls == nil {
		b.calls = map[string]int{}
	}
	b.calls[tool]++
	b.last, b.actor = r, ActorFromContext(ctx)
	return map[string]any{"tool": tool}, b.err
}

func (b *sreMaintenanceBackend) RefreshTableEvidence(ctx context.Context,
	r SREMaintenanceRequest) (any, error) {
	return b.record(ctx, "refresh", r)
}

func (b *sreMaintenanceBackend) ProposeStatistics(ctx context.Context,
	r SREMaintenanceRequest) (any, error) {
	return b.record(ctx, "propose", r)
}

func (b *sreMaintenanceBackend) RequestStatistics(ctx context.Context,
	r SREMaintenanceRequest) (any, error) {
	return b.record(ctx, "request", r)
}

func (b *sreMaintenanceBackend) RollbackStatistics(ctx context.Context,
	r SREMaintenanceRequest) (any, error) {
	return b.record(ctx, "rollback", r)
}

func (b *sreMaintenanceBackend) total() int {
	n := 0
	for _, c := range b.calls {
		n += c
	}
	return n
}

const (
	maintInv     = `"investigation_id":"11111111-1111-4111-8111-111111111111"`
	refreshArgs  = `{` + maintInv + `,"schema":"public","table":"orders"}`
	proposeStats = `{` + maintInv + `,"schema":"public","table":"orders",` +
		`"columns":["a","b"],"rationale":"columns a and b move together"}`
	requestStats  = `{"proposal_id":"22222222-2222-4222-8222-222222222222"}`
	rollbackStats = `{"proposal_id":"22222222-2222-4222-8222-222222222222","reason":"undo"}`
)

var maintenanceToolArgs = map[string]string{
	"sre_refresh_table_evidence": refreshArgs, "sre_propose_statistics": proposeStats,
	"sre_request_statistics": requestStats, "sre_rollback_statistics": rollbackStats,
}

func TestSREMaintenanceTools_ListedWithStrictSchemasAndNoSHA(t *testing.T) {
	found := map[string]Tool{}
	for _, tool := range NewServer(&sreMaintenanceBackend{}).Tools() {
		found[tool.Name] = tool
	}
	for name, required := range map[string]string{
		"sre_refresh_table_evidence": "investigation_id,schema,table",
		"sre_propose_statistics":     "investigation_id,schema,table,columns,rationale",
		"sre_request_statistics":     "proposal_id",
		"sre_rollback_statistics":    "proposal_id,reason",
	} {
		tool, ok := found[name]
		if !ok {
			t.Fatalf("%s not listed", name)
		}
		var schema struct {
			Properties           map[string]any `json:"properties"`
			Required             []string       `json:"required"`
			AdditionalProperties bool           `json:"additionalProperties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
		if strings.Join(schema.Required, ",") != required || schema.AdditionalProperties {
			t.Fatalf("%s schema = %s", name, tool.InputSchema)
		}
		for property := range schema.Properties {
			if strings.Contains(property, "sha") || strings.Contains(property, "evidence") {
				t.Fatalf("%s takes %q: the server resolves evidence itself", name, property)
			}
		}
		if _, ok := schema.Properties["database"]; !ok {
			t.Fatalf("%s has no database property (fleet)", name)
		}
	}
	for _, name := range []string{"sre_refresh_table_evidence", "sre_propose_statistics",
		"sre_request_statistics"} {
		if !strings.Contains(strings.ToLower(found[name].Description), "never execut") {
			t.Errorf("%s description does not say it never executes", name)
		}
	}
}

func TestSREMaintenanceTools_ProposeNeverCarriesASHA(t *testing.T) {
	b := &sreMaintenanceBackend{}
	s := NewServer(b)
	for _, extra := range []string{`"evidence_sha256":"` + strings.Repeat("0", 64) + `"`,
		`"evidence_id":"33333333-3333-4333-8333-333333333333"`, `"sql":"DROP TABLE x"`} {
		args := strings.TrimSuffix(proposeStats, "}") + "," + extra + "}"
		if _, rpcErr := callSRETool(t, s, operatorCtx, "sre_propose_statistics",
			args); rpcErr == nil || rpcErr["code"] != float64(-32602) {
			t.Errorf("propose with %s = %v, want -32602", extra, rpcErr)
		}
	}
	if b.total() != 0 {
		t.Fatal("a propose carrying evidence or SQL reached the backend")
	}
	if _, rpcErr := callSRETool(t, s, operatorCtx, "sre_propose_statistics",
		proposeStats); rpcErr != nil {
		t.Fatalf("propose = %v", rpcErr)
	}
	if b.calls["propose"] != 1 || b.last.Schema != "public" || b.last.Table != "orders" ||
		strings.Join(b.last.Columns, ",") != "a,b" || b.last.Rationale == "" ||
		b.last.InvestigationID != "11111111-1111-4111-8111-111111111111" ||
		b.actor != "mcp:user:2" {
		t.Fatalf("backend saw %+v as %q", b.last, b.actor)
	}
}

func TestSREMaintenanceTools_Scopes(t *testing.T) {
	for tool, want := range map[string]Scope{"sre_refresh_table_evidence": ScopePropose,
		"sre_propose_statistics": ScopePropose, "sre_request_statistics": ScopePropose,
		"sre_rollback_statistics": ScopeApprove} {
		if got, ok := RequiredScope(tool, nil); !ok || got != want {
			t.Errorf("RequiredScope(%s) = %s %v, want %s", tool, got, ok, want)
		}
	}
	proposeOnly := WithPrincipal(context.Background(), Principal{Actor: "token:t1",
		Kind: KindHuman, Scopes: []Scope{ScopeRead, ScopePropose}, TokenID: "t1"})
	b := &sreMaintenanceBackend{}
	s := NewServer(b)
	if _, rpcErr := callSRETool(t, s, proposeOnly, "sre_rollback_statistics",
		rollbackStats); rpcErr == nil || rpcErr["code"] != float64(codeScopeRequired) {
		t.Fatalf("propose-only rollback = %v, want %d", rpcErr, codeScopeRequired)
	}
	agent := agentContext(ScopeRead, ScopePropose, ScopeApprove)
	if _, rpcErr := callSRETool(t, s, agent, "sre_rollback_statistics",
		rollbackStats); rpcErr == nil || rpcErr["code"] != float64(codeApprovalReserved) {
		t.Fatalf("agent rollback = %v, want %d", rpcErr, codeApprovalReserved)
	}
	if b.total() != 0 {
		t.Fatal("a refused rollback reached the backend")
	}
	for _, tool := range []string{"sre_refresh_table_evidence", "sre_propose_statistics",
		"sre_request_statistics"} {
		if _, rpcErr := callSRETool(t, s, proposeOnly, tool,
			maintenanceToolArgs[tool]); rpcErr != nil {
			t.Errorf("propose-only %s = %v", tool, rpcErr)
		}
	}
	if b.total() != 3 || b.last.ProposalID == "" {
		t.Fatalf("propose-scope calls reached the backend %d times", b.total())
	}
	if _, rpcErr := callSRETool(t, s, proposeOnly, "sre_refresh_table_evidence",
		`{"database":"orders",`+maintInv+`,"schema":"public","table":"t"}`); rpcErr != nil ||
		b.last.Database != "orders" || b.last.Table != "t" {
		t.Fatalf("fleet refresh = %v, backend saw %+v", rpcErr, b.last)
	}
	for tool, args := range maintenanceToolArgs {
		if _, rpcErr := callSRETool(t, s, viewerCtx, tool, args); rpcErr == nil ||
			rpcErr["code"] != float64(codeScopeRequired) {
			t.Errorf("viewer %s = %v, want %d", tool, rpcErr, codeScopeRequired)
		}
	}
}

func TestSREMaintenanceTools_TypedArgumentsPerTool(t *testing.T) {
	b := &sreMaintenanceBackend{}
	s := NewServer(b)
	for tool, args := range map[string]string{
		"sre_refresh_table_evidence": `{` + maintInv + `,"schema":"public","table":"t",` +
			`"columns":["a","b"]}`,
		"sre_refresh_table_evidence ": `{` + maintInv + `,"schema":"public"}`,
		"sre_propose_statistics":      `{` + maintInv + `,"schema":"public","table":"t"}`,
		"sre_propose_statistics ": `{"schema":"public","table":"t","columns":["a","b"],` +
			`"rationale":"r"}`,
		"sre_request_statistics":   `{` + maintInv + `}`,
		"sre_request_statistics ":  `{}`,
		"sre_rollback_statistics":  requestStats,
		"sre_rollback_statistics ": `{"proposal_id":"x","reason":"  "}`,
	} {
		ctx := operatorCtx
		if strings.HasPrefix(tool, "sre_rollback") {
			ctx = WithPrincipal(context.Background(), Principal{Actor: "user:1", Role: "admin"})
		}
		if _, rpcErr := callSRETool(t, s, ctx, strings.TrimSpace(tool), args); rpcErr == nil ||
			rpcErr["code"] != float64(-32602) {
			t.Errorf("%s %s = %v, want -32602", tool, args, rpcErr)
		}
	}
	if b.total() != 0 {
		t.Fatal("invalid arguments reached the backend")
	}
}

func TestSREMaintenanceTools_ErrorsAreDistinguishable(t *testing.T) {
	for err, code := range map[error]float64{
		sre.ErrRefreshTooSoon:         -32029,
		sreaction.ErrProposalState:    -32009,
		sreaction.ErrProposalNotFound: -32004,
		sreaction.ErrPolicyBlocked:    -32010,
		ErrAdminRequired:              codeScopeRequired,
		sre.ErrInvalidRequest:         -32602,
		sre.ErrNotFound:               -32004,
		errors.New("boom"):            -32603,
	} {
		b := &sreMaintenanceBackend{err: err}
		_, rpcErr := callSRETool(t, NewServer(b), operatorCtx, "sre_request_statistics",
			requestStats)
		if rpcErr == nil || rpcErr["code"] != code {
			t.Errorf("%v = %v, want code %v", err, rpcErr, code)
		}
	}
	detailed := &sreMaintenanceBackend{err: errors.Join(sre.ErrInvalidRequest,
		errors.New("requires fresh hash-verified stat_tables evidence"))}
	_, rpcErr := callSRETool(t, NewServer(detailed), operatorCtx, "sre_request_statistics",
		requestStats)
	if msg, _ := rpcErr["message"].(string); !strings.Contains(msg, "fresh hash-verified") {
		t.Fatalf("invalid-request message = %v, want the refusal reason", rpcErr)
	}
	_, rpcErr = callSRETool(t, NewServer(&sreToolBackend{}), operatorCtx,
		"sre_request_statistics", requestStats)
	if rpcErr == nil || rpcErr["code"] != float64(-32603) {
		t.Fatalf("backend without maintenance support = %v, want -32603", rpcErr)
	}
}

// RollbackApprover maps the principal to the approving user id the
// executor records; REST parity: only an admin person may roll back.
func TestRollbackApprover(t *testing.T) {
	bind := func(p Principal) context.Context { return WithPrincipal(context.Background(), p) }
	for name, tc := range map[string]struct {
		ctx  context.Context
		want int
	}{
		"admin token":   {bind(Principal{Actor: "token:t", Role: "admin", OwnerUserID: 7}), 7},
		"admin session": {bind(Principal{Actor: "user:12", Role: "admin"}), 12},
		"operator token": {bind(Principal{Actor: "token:t", Role: "operator",
			OwnerUserID: 7}), 0},
		"admin agent": {bind(Principal{Actor: "token:a", Role: "admin", Kind: KindAgent,
			OwnerUserID: 7}), 0},
		"admin without a user": {bind(Principal{Actor: "token:t", Role: "admin"}), 0},
		"non-numeric user":     {bind(Principal{Actor: "user:abc", Role: "admin"}), 0},
		"user zero":            {bind(Principal{Actor: "user:0", Role: "admin"}), 0},
		"negative user":        {bind(Principal{Actor: "user:-4", Role: "admin"}), 0},
		"unbound":              {context.Background(), 0},
	} {
		got, err := RollbackApprover(tc.ctx)
		if tc.want > 0 && (err != nil || got != tc.want) {
			t.Errorf("%s = %d (%v), want %d", name, got, err, tc.want)
		}
		if tc.want == 0 && (!errors.Is(err, ErrAdminRequired) || got != 0) {
			t.Errorf("%s = %d (%v), want ErrAdminRequired", name, got, err)
		}
	}
}
