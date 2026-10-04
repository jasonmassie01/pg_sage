package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Roadmap 2.1: sre_get_transcript gives an external agent the
// tool-calling investigator's transcript of one investigation, redacted
// like the replay export. Read-only, typed arguments; keeping
// identifiers is an operator opt-in.

type transcriptBackend struct {
	sreToolBackend
	lastKeep bool
}

func (b *transcriptBackend) GetTranscript(_ context.Context, r InvestigationRequest,
	keep bool) (any, error) {
	b.last, b.lastKeep = r, keep
	return map[string]any{"schema": sre.TranscriptSchema, "id": r.InvestigationID}, b.err
}

func TestTranscriptTool_ListedReadOnlyAndStrict(t *testing.T) {
	s := NewServer(&transcriptBackend{})
	for _, tool := range s.Tools() {
		if tool.Name != "sre_get_transcript" {
			continue
		}
		var schema map[string]any
		if json.Unmarshal(tool.InputSchema, &schema) != nil ||
			schema["additionalProperties"] != false {
			t.Fatalf("schema is not strict: %s", tool.InputSchema)
		}
		if mutatingTools[tool.Name] {
			t.Fatal("sre_get_transcript is marked mutating")
		}
		return
	}
	t.Fatal("tools/list lacks sre_get_transcript")
}

func TestTranscriptTool_ViewerReadsTheRedactedTranscript(t *testing.T) {
	b := &transcriptBackend{}
	s := NewServer(b)
	res, rpcErr := callSRETool(t, s, viewerCtx, "sre_get_transcript",
		`{"database":"orders","investigation_id":"11111111-1111-4111-8111-111111111111"}`)
	if rpcErr != nil || res == nil || b.last.Database != "orders" || b.lastKeep {
		t.Fatalf("transcript = %v %v, backend saw %+v keep=%v", res, rpcErr, b.last,
			b.lastKeep)
	}
}

func TestTranscriptTool_KeepIdentifiersNeedsAnOperator(t *testing.T) {
	b := &transcriptBackend{}
	s := NewServer(b)
	args := `{"investigation_id":"x","keep_identifiers":true}`
	if _, rpcErr := callSRETool(t, s, viewerCtx, "sre_get_transcript",
		args); rpcErr == nil {
		t.Fatal("a viewer kept identifiers")
	}
	op := WithPrincipal(context.Background(), Principal{Actor: "user:1", Role: "operator"})
	if _, rpcErr := callSRETool(t, s, op, "sre_get_transcript", args); rpcErr != nil ||
		!b.lastKeep {
		t.Fatalf("operator keep = %v, backend keep %v", rpcErr, b.lastKeep)
	}
}

func TestTranscriptTool_InvalidArgumentsAndErrors(t *testing.T) {
	s := NewServer(&transcriptBackend{})
	for _, args := range []string{`{}`, `{"investigation_id":"x","sql":"x"}`,
		`{"investigation_id":"x","keep_identifiers":"yes"}`} {
		if _, rpcErr := callSRETool(t, s, viewerCtx, "sre_get_transcript", args); rpcErr == nil ||
			rpcErr["code"] != float64(-32602) {
			t.Errorf("%s = %v, want -32602", args, rpcErr)
		}
	}
	for err, code := range map[error]float64{sre.ErrNoTranscript: -32004,
		sre.ErrNotFound: -32004, errors.New("boom"): -32603} {
		b := &transcriptBackend{}
		b.err = err
		if _, rpcErr := callSRETool(t, NewServer(b), viewerCtx, "sre_get_transcript",
			`{"investigation_id":"x"}`); rpcErr == nil || rpcErr["code"] != code {
			t.Errorf("%v -> %v, want %v", err, rpcErr, code)
		}
	}
	if _, rpcErr := callSRETool(t, NewServer(&sreToolBackend{}), viewerCtx,
		"sre_get_transcript", `{"investigation_id":"x"}`); rpcErr == nil {
		t.Fatal("a backend without transcripts answered")
	}
}

func TestTranscriptTool_ProductionBackendDelegates(t *testing.T) {
	inv := &transcriptBackend{}
	b := &ProductionBackend{dependencies: ProductionDependencies{Investigations: inv}}
	if _, err := b.GetTranscript(context.Background(),
		InvestigationRequest{InvestigationID: "x"}, true); err != nil ||
		inv.last.InvestigationID != "x" || !inv.lastKeep {
		t.Fatalf("delegation: %v %+v", err, inv.last)
	}
	plain := &ProductionBackend{dependencies: ProductionDependencies{
		Investigations: &sreToolBackend{}}}
	if _, err := plain.GetTranscript(context.Background(), InvestigationRequest{},
		false); !errors.Is(err, ErrProductionDependencyUnavailable) {
		t.Fatalf("without transcripts = %v", err)
	}
}
