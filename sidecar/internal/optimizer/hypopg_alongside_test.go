package optimizer

import (
	"context"
	"testing"
	"time"
)

// A candidate is measured with the in-flight indexes of its table present
// as hypothetical indexes: next to an in-flight index that already serves
// the lookup it gains nothing (lifeos 1.10.0: two overlapping indexes were
// each verified on their own).
func TestHypoPGMeasuresWithInFlightIndexes(t *testing.T) {
	pool := hypopgSessionPool(t)
	h := NewHypoPG(pool, noopLog2)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	queries := []QueryInfo{{QueryID: 1,
		Text: "SELECT id FROM hypopg_session_test.items WHERE category=42"}}
	rec := Recommendation{DDL: "CREATE INDEX candidate ON hypopg_session_test.items " +
		"(category, id)"}
	alone, err := h.Validate(ctx, rec, queries)
	if err != nil || alone.Improvement <= 1 {
		t.Fatalf("alone: %+v %v", alone, err)
	}
	rec.Alongside = []string{"CREATE INDEX inflight ON hypopg_session_test.items (category)"}
	with, err := h.Validate(ctx, rec, queries)
	if err != nil || with.Improvement >= alone.Improvement/2 {
		t.Fatalf("with the in-flight index: %+v %v (alone %.1f%%)", with, err,
			alone.Improvement)
	}
	assertHypoPGSessionClean(t, pool)
	rec.Alongside = []string{"CREATE INDEX broken ON hypopg_session_test.items (nope)"}
	if _, err := h.Validate(ctx, rec, queries); err == nil {
		t.Fatal("an in-flight index that cannot be built hypothetically fails the what-if")
	}
	assertHypoPGSessionClean(t, pool)
}
