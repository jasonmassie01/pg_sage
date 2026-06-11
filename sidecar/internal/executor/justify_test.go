package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

func TestBuildJustificationPrompt(t *testing.T) {
	f := analyzer.Finding{
		Category:         "autovacuum_tuning",
		ObjectIdentifier: "public.orders",
		Title:            "Table public.orders should use scale factor 0.02",
		Recommendation:   "Lower the scale factor.",
		RecommendedSQL:   `ALTER TABLE "public"."orders" SET (autovacuum_vacuum_scale_factor = 0.02);`,
		RollbackSQL:      `ALTER TABLE "public"."orders" RESET (autovacuum_vacuum_scale_factor);`,
	}
	p := buildJustificationPrompt(f)
	for _, want := range []string{"autovacuum_tuning", "public.orders", "Rollback SQL:", "Executed SQL:"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	// No-rollback case gets a sensible default.
	p2 := buildJustificationPrompt(analyzer.Finding{Category: "stale_statistics", RecommendedSQL: "ANALYZE x;"})
	if !strings.Contains(p2, "no rollback needed") {
		t.Errorf("expected no-rollback note, got:\n%s", p2)
	}
}

type fakeJustifier struct{ called bool }

func (f *fakeJustifier) Chat(_ context.Context, _, _ string, _ int) (string, int, error) {
	f.called = true
	return "Created an index to speed up the orders lookup; reversible by dropping it.", 42, nil
}

func TestWithJustifier_SetsField(t *testing.T) {
	e := &Executor{}
	if e.justifier != nil {
		t.Fatal("justifier should start nil")
	}
	fj := &fakeJustifier{}
	e.WithJustifier(fj)
	if e.justifier == nil {
		t.Error("WithJustifier did not set the justifier")
	}
}
