package sre

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The transcript surface (API and MCP): the plan, each tool call with
// its redacted result and digest, the citations and the outcome.
// Redaction is the replay export's (#111): identifiers become keyed
// hashes unless the operator keeps them; secrets never survive.

func secretRunner() *scriptedRunner {
	r := idleChainRunner()
	r.script(probes.LongTransactions, rows(probes.LongTransactions, probes.Row{
		"pid": int64(4242), "state": "idle in transaction", "xact_age_s": 95.0,
		"application_name": "postgres://svc:CanaryTx-91@db.internal/app",
		"relation":         "public.orders"}))
	return r
}

func transcriptInvestigation(t *testing.T) (*Service, Investigation) {
	t.Helper()
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t,
		func(w http.ResponseWriter, body string) {
			writeCompletion(w, map[string]any{"role": "assistant",
				"content": "Plan: re-read long transactions for public.orders.",
				"tool_calls": []map[string]any{{"id": "c1", "type": "function",
					"function": map[string]any{"name": ToolRunProbe,
						"arguments": probeArgs(probes.LongTransactions)}}}})
		},
		submit(func(body string) invFinal {
			return invFinal{Outcome: "agree", Claims: []invClaim{
				idleClaim(aliasOf(t, body, probes.LockGraph, "ok"))}}
		}))
	c, _ := investigatorCoordinator(t, ctx, st, secretRunner(), m.client(), invOptions{})
	inv := startAndRun(t, ctx, c, lockTrigger("inv-transcript"))
	return NewService("db1", c, st), inv
}

func TestTranscript_RedactsResultsByDefault(t *testing.T) {
	svc, inv := transcriptInvestigation(t)
	view, err := svc.Transcript(t.Context(), inv.ID, TranscriptOptions{})
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if view.Schema != TranscriptSchema || view.Plan != PlanNarrow || view.IdentifiersKept ||
		len(view.Steps) < 2 || view.Stop != "final" {
		t.Fatalf("view = %+v", view)
	}
	raw, _ := json.Marshal(view)
	text := string(raw)
	if strings.Contains(text, "CanaryTx-91") || strings.Contains(text, "public.orders") {
		t.Fatalf("the transcript leaks an identifier or a secret: %s", text)
	}
	probe := view.Steps[0]
	if probe.Tool != ToolRunProbe || probe.Result == nil || len(probe.Result.Rows) != 1 ||
		probe.Digest == "" || probe.EvidenceID == "" {
		t.Fatalf("probe step = %+v", probe)
	}
	if v := probe.Result.Rows[0]["xact_age_s"]; v == nil {
		t.Fatalf("numbers must survive redaction: %+v", probe.Result.Rows[0])
	}
	if len(view.Claims) != 1 || len(view.Claims[0].EvidenceIDs) != 1 {
		t.Fatalf("claims = %+v", view.Claims)
	}
	if view.Outcome == nil || view.Outcome.Outcome != ModelAgreed {
		t.Fatalf("outcome = %+v", view.Outcome)
	}
}

func TestTranscript_KeepIdentifiersStillScrubsSecrets(t *testing.T) {
	svc, inv := transcriptInvestigation(t)
	view, err := svc.Transcript(t.Context(), inv.ID, TranscriptOptions{KeepIdentifiers: true})
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	raw, _ := json.Marshal(view)
	text := string(raw)
	if !view.IdentifiersKept || !strings.Contains(text, "public.orders") ||
		strings.Contains(text, "CanaryTx-91") {
		t.Fatalf("keep_identifiers view: %s", text)
	}
	if !strings.Contains(view.ModelPlan, "public.orders") {
		t.Fatalf("model plan = %q", view.ModelPlan)
	}
}

func TestTranscript_WithoutAnInvestigatorRun(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	inv := startAndRun(t, ctx, c, lockTrigger("inv-no-transcript"))
	svc := NewService("db1", c, st)
	if _, err := svc.Transcript(ctx, inv.ID, TranscriptOptions{}); !errors.Is(err,
		ErrNoTranscript) {
		t.Fatalf("deterministic investigation: %v, want ErrNoTranscript", err)
	}
	if _, err := svc.Transcript(ctx, NewUUID(), TranscriptOptions{}); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("unknown investigation: %v, want ErrNotFound", err)
	}
	if _, err := svc.Transcript(ctx, "not-a-uuid", TranscriptOptions{}); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("invalid id: %v, want ErrInvalidRequest", err)
	}
}

func TestExportMarkdown_ShowsTheInvestigator(t *testing.T) {
	svc, inv := transcriptInvestigation(t)
	md, err := svc.ExportMarkdown(t.Context(), inv.ID)
	if err != nil {
		t.Fatalf("ExportMarkdown: %v", err)
	}
	if !containsAll(md, "## Investigator (model-generated", "plan narrow", ToolRunProbe,
		"outcome agreed") {
		t.Fatalf("markdown misses the investigator section:\n%s", md)
	}
}
