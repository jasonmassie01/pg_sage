package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/earned/packetreview"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Phase 1.1: an operator's agent can review a finished investigation
// (sre_review_investigation) and ask pg_sage to evaluate promotions
// (sre_evaluate_autonomy). Both are operator+ writes; neither approves a
// promotion, which stays a human step in the UI/API.

func (b *autonomyToolBackend) ReviewInvestigation(_ context.Context, r AutonomyRequest,
	actor string) (any, error) {
	b.got = append(b.got, r)
	b.actor = actor
	return map[string]any{"family": "lock_blocking"}, b.err
}

func (b *autonomyToolBackend) EvaluateAutonomy(_ context.Context, r AutonomyRequest) (any,
	error) {
	b.got = append(b.got, r)
	return map[string]any{"created": []any{}, "not_proposed": []any{}}, b.err
}

const reviewArgs = `{"database":"orders",` +
	`"investigation_id":"11111111-1111-4111-8111-111111111111","verdict":"rejected",` +
	`"note":"wrong holder","actual_root_cause":"connection_leak"}`

func TestReviewAndEvaluateToolsAreListedAndStrict(t *testing.T) {
	found := map[string]Tool{}
	for _, tool := range NewServer(&autonomyToolBackend{}).Tools() {
		found[tool.Name] = tool
	}
	for _, name := range []string{"sre_review_investigation", "sre_evaluate_autonomy"} {
		tool, ok := found[name]
		if !ok {
			t.Fatalf("%s is not listed", name)
		}
		if !strings.Contains(string(tool.InputSchema), `"additionalProperties":false`) {
			t.Errorf("%s schema is not strict", name)
		}
	}
	if !strings.Contains(string(found["sre_review_investigation"].InputSchema),
		`"enum":["accepted","rejected"]`) {
		t.Fatalf("review verdict is not an enum: %s",
			found["sre_review_investigation"].InputSchema)
	}
}

func TestReviewToolNeedsAnOperatorAndPassesTheRequest(t *testing.T) {
	b := &autonomyToolBackend{}
	if _, rpcErr := callSRETool(t, NewServer(b), viewerCtx, "sre_review_investigation",
		reviewArgs); rpcErr == nil || rpcErr["code"] != float64(-32001) || len(b.got) != 0 {
		t.Fatalf("viewer review = %v (%+v)", rpcErr, b.got)
	}
	if _, rpcErr := callSRETool(t, NewServer(b), context.Background(),
		"sre_review_investigation", reviewArgs); rpcErr == nil || len(b.got) != 0 {
		t.Fatalf("unbound review = %v", rpcErr)
	}
	result, rpcErr := callSRETool(t, NewServer(b), autonomyOperatorCtx,
		"sre_review_investigation", reviewArgs)
	if rpcErr != nil || result["structuredContent"] == nil || len(b.got) != 1 ||
		b.actor != "mcp:user:2" {
		t.Fatalf("operator review = %v %v actor=%q", result, rpcErr, b.actor)
	}
	r := b.got[0]
	if r.Database != "orders" || r.InvestigationID != "11111111-1111-4111-8111-111111111111" ||
		r.Verdict != "rejected" || r.Note != "wrong holder" ||
		r.ActualRootCause != "connection_leak" {
		t.Fatalf("request = %+v", r)
	}
}

func TestEvaluateToolNeedsAnOperator(t *testing.T) {
	b := &autonomyToolBackend{}
	if _, rpcErr := callSRETool(t, NewServer(b), viewerCtx, "sre_evaluate_autonomy",
		`{"database":"orders"}`); rpcErr == nil || rpcErr["code"] != float64(-32001) {
		t.Fatalf("viewer evaluate = %v", rpcErr)
	}
	result, rpcErr := callSRETool(t, NewServer(b), autonomyOperatorCtx, "sre_evaluate_autonomy",
		`{"database":"orders"}`)
	if rpcErr != nil || result["structuredContent"] == nil || len(b.got) != 1 ||
		b.got[0].Database != "orders" {
		t.Fatalf("operator evaluate = %v %v (%+v)", result, rpcErr, b.got)
	}
}

func TestReviewToolRejectsBadArguments(t *testing.T) {
	b := &autonomyToolBackend{}
	s := NewServer(b)
	for name, args := range map[string]string{
		"no verdict": `{"investigation_id":"11111111-1111-4111-8111-111111111111"}`,
		"no id":      `{"verdict":"accepted"}`,
		"bad verdict": `{"investigation_id":"11111111-1111-4111-8111-111111111111",` +
			`"verdict":"confirmed"}`,
		"reviewer is not an argument": `{"investigation_id":` +
			`"11111111-1111-4111-8111-111111111111","verdict":"accepted","reviewer":"x"}`,
		"downgrade fields": `{"investigation_id":"11111111-1111-4111-8111-111111111111",` +
			`"verdict":"accepted","level":"L0"}`,
	} {
		if _, rpcErr := callSRETool(t, s, autonomyOperatorCtx, "sre_review_investigation",
			args); rpcErr == nil || rpcErr["code"] != float64(-32602) {
			t.Errorf("%s: %v", name, rpcErr)
		}
	}
	if _, rpcErr := callSRETool(t, s, autonomyOperatorCtx, "sre_evaluate_autonomy",
		`{"family":"wal_retention"}`); rpcErr == nil || rpcErr["code"] != float64(-32602) {
		t.Errorf("evaluate with a family: %v", rpcErr)
	}
	if len(b.got) != 0 {
		t.Fatalf("bad arguments reached the backend: %+v", b.got)
	}
}

func TestReviewToolMapsErrors(t *testing.T) {
	for err, code := range map[error]float64{
		earned.ErrInvalidRequest:      -32602,
		earned.ErrConflict:            -32602,
		packetreview.ErrNotFinished:   -32602,
		sre.ErrNotFound:               -32004,
		packetreview.ErrUnavailable:   -32603,
		earned.ErrUnavailable:         -32603,
	} {
		b := &autonomyToolBackend{err: err}
		if _, rpcErr := callSRETool(t, NewServer(b), autonomyOperatorCtx,
			"sre_review_investigation", reviewArgs); rpcErr == nil || rpcErr["code"] != code {
			t.Errorf("%v -> %v, want %v", err, rpcErr, code)
		}
	}
}
