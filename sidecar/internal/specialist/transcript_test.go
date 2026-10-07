package specialist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// The redacted investigator transcript through the contract (revision
// 1.1.0): read scope, the caller's databases only, pg_sage's own redaction
// (keep_identifiers honoured), not_found when the investigator never ran.

func transcriptView(id sre.UUID) sre.TranscriptView {
	return sre.TranscriptView{Schema: sre.TranscriptSchema, Database: "orders",
		InvestigationID: id, Plan: sre.PlanNarrow, Protocol: "native", Stop: "final",
		ModelCalls: 3, Probes: 2, Claims: []sre.NarrativeClaim{{Text: "pid 4242 blocks",
			EvidenceIDs: []sre.UUID{ev1}}},
		Steps: []sre.TranscriptStep{{InvestigatorStep: sre.InvestigatorStep{Seq: 1,
			Tool: sre.ToolRunProbe, Status: "ok", EvidenceID: ev1}}},
		Redaction: []string{"identifiers_kept"}, IdentifiersKept: true}
}

func TestTranscript_ServedToAReader(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	h.orders.put(lockDetail())
	h.orders.transcripts = map[sre.UUID]sre.TranscriptView{inv: transcriptView(inv)}
	resp, err := h.svc.Transcript(context.Background(), reader, "orders", string(inv))
	if err != nil {
		t.Fatal(err)
	}
	if resp.ContractVersion != ContractVersion || resp.Database != "orders" ||
		resp.InvestigationID != string(inv) {
		t.Fatalf("header %+v", resp)
	}
	tr := resp.Transcript
	if tr["schema"] != sre.TranscriptSchema || tr["plan"] != sre.PlanNarrow ||
		tr["stop"] != "final" {
		t.Fatalf("transcript document %+v", tr)
	}
	steps, ok := tr["steps"].([]any)
	if !ok || len(steps) != 1 || steps[0].(map[string]any)["evidence_id"] != string(ev1) {
		t.Fatalf("steps %+v", tr["steps"])
	}
	if mc, ok := tr["model_calls"].(json.Number); !ok || mc.String() != "3" {
		t.Fatalf("numbers stay exact: %#v", tr["model_calls"])
	}
	if len(h.orders.transcriptKeep) != 1 || !h.orders.transcriptKeep[0] {
		t.Fatalf("keep_identifiers reaches the redactor: %v", h.orders.transcriptKeep)
	}
}

func TestTranscript_Refusals(t *testing.T) {
	h := newHarness(t, DefaultLimits())
	h.orders.transcripts = map[sre.UUID]sre.TranscriptView{}
	ctx := context.Background()
	noRead := Identity{TokenID: "t", Scopes: []string{"propose"},
		Databases: []string{"orders"}}
	cases := []struct {
		name string
		id   Identity
		db   string
		inv  string
		want error
	}{
		{"scope", noRead, "orders", string(inv), ErrScope},
		{"database", reader, "billing", string(inv), ErrDatabaseNotPermitted},
		{"bad id", reader, "orders", "1; DROP TABLE x", ErrInvalid},
		{"never ran", reader, "orders", string(inv), ErrNotFound},
	}
	for _, c := range cases {
		_, err := h.svc.Transcript(ctx, c.id, c.db, c.inv)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if len(h.orders.transcriptKeep) != 1 {
		t.Fatalf("only the admitted, well-formed call reaches the backend: %d",
			len(h.orders.transcriptKeep))
	}
	h.orders.transcriptErr = fmt.Errorf("store: %w", sre.ErrMetadataUnavailable)
	if _, err := h.svc.Transcript(ctx, reader, "orders", string(inv)); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("store outage: %v", err)
	}
}

func TestTranscript_HTTPRoute(t *testing.T) {
	handler, h := newTestHandler(t, DefaultLimits())
	h.orders.transcripts = map[sre.UUID]sre.TranscriptView{inv: transcriptView(inv)}
	path := base + "/databases/orders/investigations/" + string(inv) + "/transcript"
	w := call(t, handler, http.MethodGet, path, "read-token", "")
	if w.Code != http.StatusOK || w.Header().Get("X-Sage-Contract-Version") !=
		ContractVersion {
		t.Fatalf("transcript %d %s", w.Code, w.Body.String())
	}
	var resp TranscriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil ||
		resp.Transcript["schema"] != sre.TranscriptSchema {
		t.Fatalf("body %s (%v)", w.Body.String(), err)
	}
	if w := call(t, handler, http.MethodGet, path, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	missing := base + "/databases/orders/investigations/" + string(ev2) + "/transcript"
	w = call(t, handler, http.MethodGet, missing, "read-token", "")
	if w.Code != http.StatusNotFound || errorBody(t, w).Code != "not_found" {
		t.Fatalf("no transcript: %d %s", w.Code, w.Body.String())
	}
}
