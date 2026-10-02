package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Sage SRE runbook and memory tools (AI-SRE-SPEC §7.1, §9). Viewers list
// and read runbooks, run history and similar past incidents; operators
// write drafts and compile English into drafts. No MCP tool signs a
// runbook: a signature is a human act through the authenticated UI/API,
// never something an agent can do on its own.

type runbookToolBackend struct {
	sreToolBackend
	method string
	last   RunbookRequest
	actor  string
	err    error
}

func (b *runbookToolBackend) record(ctx context.Context, m string, r RunbookRequest) (any,
	error) {
	b.method, b.last, b.actor = m, r, ActorFromContext(ctx)
	return map[string]any{"method": m}, b.err
}

func (b *runbookToolBackend) ListRunbooks(ctx context.Context, r RunbookRequest) (any, error) {
	return b.record(ctx, "list", r)
}

func (b *runbookToolBackend) GetRunbook(ctx context.Context, r RunbookRequest) (any, error) {
	return b.record(ctx, "get", r)
}

func (b *runbookToolBackend) RunbookRuns(ctx context.Context, r RunbookRequest) (any, error) {
	return b.record(ctx, "runs", r)
}

func (b *runbookToolBackend) DraftRunbook(ctx context.Context, r RunbookRequest) (any, error) {
	return b.record(ctx, "draft", r)
}

func (b *runbookToolBackend) CompileRunbook(ctx context.Context,
	r RunbookRequest) (any, error) {
	return b.record(ctx, "compile", r)
}

func (b *runbookToolBackend) SimilarIncidents(ctx context.Context,
	r RunbookRequest) (any, error) {
	return b.record(ctx, "similar", r)
}

var operatorCtx = WithPrincipal(context.Background(), Principal{Actor: "user:2",
	Role: "operator"})

const rbID = "11111111-1111-4111-8111-111111111111"

func TestRunbookTools_ListedStrictAndNeverSign(t *testing.T) {
	s := NewServer(&runbookToolBackend{})
	found := map[string]Tool{}
	for _, tool := range s.Tools() {
		found[tool.Name] = tool
		if strings.Contains(tool.Name, "sign") {
			t.Errorf("tool %s signs runbooks; signing is human-only", tool.Name)
		}
	}
	for _, name := range []string{"sre_list_runbooks", "sre_get_runbook",
		"sre_runbook_runs", "sre_similar_incidents", "sre_draft_runbook",
		"sre_compile_runbook"} {
		tool, ok := found[name]
		if !ok {
			t.Fatalf("tools/list lacks %s", name)
		}
		var schema map[string]any
		if json.Unmarshal(tool.InputSchema, &schema) != nil ||
			schema["additionalProperties"] != false {
			t.Errorf("%s schema is not strict: %s", name, tool.InputSchema)
		}
	}
}

func TestRunbookTools_ViewerReads(t *testing.T) {
	b := &runbookToolBackend{}
	s := NewServer(b)
	cases := []struct{ tool, args, method string }{
		{"sre_list_runbooks", `{"database":"orders"}`, "list"},
		{"sre_get_runbook", `{"runbook_id":"` + rbID + `"}`, "get"},
		{"sre_runbook_runs", `{"database":"orders","runbook_id":"` + rbID + `"}`, "runs"},
		{"sre_similar_incidents", `{"investigation_id":"` + rbID + `"}`, "similar"},
	}
	for _, c := range cases {
		res, rpcErr := callSRETool(t, s, viewerCtx, c.tool, c.args)
		if rpcErr != nil || res == nil || b.method != c.method {
			t.Errorf("%s = %v %v (backend %s), want %s", c.tool, res, rpcErr, b.method,
				c.method)
		}
	}
	if b.last.InvestigationID != rbID {
		t.Fatalf("similar saw %+v", b.last)
	}
}

func TestRunbookTools_WritesNeedAnOperator(t *testing.T) {
	def := `{"definition":{"name":"x"}}`
	for _, ctx := range []context.Context{viewerCtx, context.Background()} {
		b := &runbookToolBackend{}
		s := NewServer(b)
		for tool, args := range map[string]string{"sre_draft_runbook": def,
			"sre_compile_runbook": `{"text":"read the locks"}`} {
			if _, rpcErr := callSRETool(t, s, ctx, tool, args); rpcErr == nil ||
				rpcErr["code"] != float64(-32001) || b.method != "" {
				t.Errorf("%s without an operator = %v (backend %q), want -32001", tool,
					rpcErr, b.method)
			}
		}
	}
	b := &runbookToolBackend{}
	s := NewServer(b)
	if _, rpcErr := callSRETool(t, s, operatorCtx, "sre_draft_runbook",
		`{"database":"orders","runbook_id":"`+rbID+`","base_version":2,`+
			`"definition":{"name":"x"}}`); rpcErr != nil || b.method != "draft" ||
		b.last.BaseVersion != 2 || string(b.last.Definition) != `{"name":"x"}` ||
		b.actor != "mcp:user:2" {
		t.Fatalf("operator draft = %v, backend saw %+v as %s", rpcErr, b.last, b.actor)
	}
	if _, rpcErr := callSRETool(t, s, operatorCtx, "sre_compile_runbook",
		`{"text":"read the locks"}`); rpcErr != nil || b.method != "compile" ||
		b.last.Text != "read the locks" {
		t.Fatalf("operator compile = %v, backend saw %+v", rpcErr, b.last)
	}
}

func TestRunbookTools_RejectInvalidArguments(t *testing.T) {
	s := NewServer(&runbookToolBackend{})
	for name, args := range map[string]string{
		"sre_get_runbook":       `{}`,
		"sre_runbook_runs":      `{"database":"orders"}`,
		"sre_similar_incidents": `{}`,
		"sre_list_runbooks":     `{"sql":"x"}`,
	} {
		if _, rpcErr := callSRETool(t, s, viewerCtx, name, args); rpcErr == nil ||
			rpcErr["code"] != float64(-32602) {
			t.Errorf("%s %s = %v, want -32602", name, args, rpcErr)
		}
	}
	for name, args := range map[string]string{
		"sre_draft_runbook":   `{"database":"orders"}`,
		"sre_compile_runbook": `{"text":""}`,
	} {
		if _, rpcErr := callSRETool(t, s, operatorCtx, name, args); rpcErr == nil ||
			rpcErr["code"] != float64(-32602) {
			t.Errorf("%s %s = %v, want -32602", name, args, rpcErr)
		}
	}
	revise := `{"runbook_id":"` + rbID + `","definition":{"name":"x"}}`
	if _, rpcErr := callSRETool(t, s, operatorCtx, "sre_draft_runbook", revise); rpcErr == nil ||
		rpcErr["code"] != float64(-32602) {
		t.Errorf("revise without base_version = %v, want -32602", rpcErr)
	}
}

func TestRunbookTools_ErrorsAreDistinguishable(t *testing.T) {
	cases := map[error]float64{sre.ErrRunbookNotFound: -32004,
		sre.ErrVersionConflict: -32009, sre.ErrHashMismatch: -32009,
		sre.ErrRetired: -32009, sre.ErrModelUnavailable: -32010,
		&runbook.Rejection{Reason: "invalid_definition"}: -32022,
		sre.ErrInvalidRequest:                            -32602, errors.New("boom"): -32603}
	for err, code := range cases {
		s := NewServer(&runbookToolBackend{err: err})
		_, rpcErr := callSRETool(t, s, operatorCtx, "sre_compile_runbook",
			`{"text":"read the locks"}`)
		if rpcErr == nil || rpcErr["code"] != code {
			t.Errorf("%v -> %v, want code %v", err, rpcErr, code)
		}
	}
}

func TestRunbookTools_BackendWithoutRunbooks(t *testing.T) {
	s := NewServer(&sreToolBackend{})
	if _, rpcErr := callSRETool(t, s, viewerCtx, "sre_list_runbooks", `{}`); rpcErr == nil ||
		rpcErr["code"] != float64(-32603) {
		t.Fatalf("a backend without runbooks = %v, want -32603", rpcErr)
	}
	inv := &runbookToolBackend{}
	b := &ProductionBackend{dependencies: ProductionDependencies{Investigations: inv}}
	if _, err := b.ListRunbooks(context.Background(), RunbookRequest{Database: "x"}); err != nil ||
		inv.method != "list" {
		t.Fatalf("production delegation = %v (%s)", err, inv.method)
	}
	plain := &ProductionBackend{dependencies: ProductionDependencies{
		Investigations: &sreToolBackend{}}}
	if _, err := plain.CompileRunbook(context.Background(), RunbookRequest{}); !errors.Is(err,
		ErrProductionDependencyUnavailable) {
		t.Fatalf("production backend without runbooks = %v", err)
	}
}
