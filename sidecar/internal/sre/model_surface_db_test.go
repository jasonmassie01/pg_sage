package sre

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Surfaces (AI-SRE-SPEC §9, CHECK-41): the detail behind the API and MCP
// tools and the export show the model ranking separately, labeled "model
// ranking", with the model turn count, and the narrative labeled as
// model-generated. Model text is redacted like everything else.

func secretClaimReview(t *testing.T) fakeReply {
	return toolReply(func(body string) string {
		return wireReview{Ranking: idleRanking(), Claims: []wireClaim{{
			Text:        "pid 4242 blocks 2 sessions; password=hunter2 was in the app name.",
			EvidenceIDs: []string{aliasOf(t, body, probes.LockGraph, "ok")}}}}.json()
	})
}

func TestModelSurface_DetailShowsLabeledModelOutput(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, secretClaimReview(t))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-surface"))
	d, err := NewService("orders", c, st).Detail(ctx, inv.ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	raw, _ := json.Marshal(d)
	body := string(raw)
	for _, want := range []string{`"model_ranking":{`, `"label":"model ranking"`,
		`"narrative":{`, `"label":"model-generated narrative"`, `"model_turns":1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, "hunter2") {
		t.Fatalf("detail leaks a credential from model text: %s", body)
	}
	for _, h := range d.Hypotheses {
		if h.Node == "idle_in_tx_holder" && h.Status != HypothesisRoot {
			t.Fatalf("hypotheses were reordered by the model: %+v", d.Hypotheses)
		}
	}
}

func TestModelSurface_ExportLabelsModelSections(t *testing.T) {
	st, _, ctx := liveStore(t, budgetLimits())
	m := newFakeModel(t, secretClaimReview(t))
	c, _ := modelCoordinator(t, ctx, st, idleChainRunner(), m.client())
	inv := startAndRun(t, ctx, c, lockTrigger("m3-export"))
	svc := NewService("orders", c, st)
	doc, err := svc.Export(ctx, inv.ID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	raw, _ := json.Marshal(doc)
	assertNoSecrets(t, "JSON export", raw)
	if doc.Investigation.Summary.ModelRanking == nil ||
		doc.Investigation.Summary.Narrative == nil {
		t.Fatalf("export lacks the model output: %+v", doc.Investigation.Summary)
	}
	md, err := svc.ExportMarkdown(ctx, inv.ID)
	if err != nil {
		t.Fatalf("markdown: %v", err)
	}
	assertNoSecrets(t, "Markdown export", []byte(md))
	for _, want := range []string{"- Model turns: 1", "## Model ranking (model-generated",
		"1. idle_in_tx_holder", "## Narrative (model-generated", "pid 4242 blocks 2"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, md)
		}
	}
	likely := strings.Index(md, "## Likely explanation")
	model := strings.Index(md, "## Model ranking")
	if likely < 0 || model < likely {
		t.Fatalf("the model ranking must follow the deterministic diagnosis:\n%s", md)
	}
}

// A deterministic investigation's export has no model sections.
func TestModelSurface_DeterministicExportHasNoModelSections(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	c, _ := testCoordinator(t, ctx, st, idleChainRunner(), nil)
	inv := startAndRun(t, ctx, c, lockTrigger("m3-plain-export"))
	md, err := NewService("orders", c, st).ExportMarkdown(ctx, inv.ID)
	if err != nil {
		t.Fatalf("markdown: %v", err)
	}
	if strings.Contains(md, "Model ranking") || strings.Contains(md, "Narrative") ||
		!strings.Contains(md, "- Model turns: 0") {
		t.Fatalf("deterministic export:\n%s", md)
	}
}
