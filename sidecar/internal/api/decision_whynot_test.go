package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/ledger"
)

// ux-08b: each "Held back" row names the why-not link its reason fails
// (why_not.link) with the chain's sentence, so the row's reason line and
// the Why not panel opened from it agree. A reason with no link (an
// older release's sentence) has no why_not and the row keeps its words.
// No concurrency test beyond the route's own: the mapping is a pure read.

func TestDecisionsRowsNameTheirWhyNotLink(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	window := budgetEntry(8, now.Add(-time.Minute))
	window.Reason, window.BudgetDetail = "outside_maintenance_window", ""
	sentence := budgetEntry(9, now.Add(-2*time.Minute))
	sentence.Reason, sentence.BudgetDetail = "live DDL preflight has warnings", ""
	fakeDecisionReads(t, map[string][]ledger.WithheldEntry{
		"a": {budgetEntry(7, now), window, sentence}}, nil)
	rec := requestPolicy(decisionsMux(decisionsFleet(t, "a")), http.MethodGet,
		"/api/v1/decisions?database=a", "", testOperatorUser())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	rows := decodeDecisions(t, rec.Body.Bytes()).Decisions
	if len(rows) != 3 {
		t.Fatalf("rows = %v, want 3", rows)
	}
	wantLinks := []struct{ link, sentence string }{
		{"budget", "The change budget for this window is spent."},
		{"window", "Outside the maintenance window."},
	}
	for i, want := range wantLinks {
		got, ok := rows[i]["why_not"].(map[string]any)
		if !ok || got["link"] != want.link || got["sentence"] != want.sentence {
			t.Fatalf("row %d why_not = %v, want %+v (row %v)", i, rows[i]["why_not"],
				want, rows[i])
		}
	}
	if _, has := rows[2]["why_not"]; has {
		t.Fatalf("a sentence reason names a link: %v", rows[2])
	}
	if rows[2]["human_reason"] == "" {
		t.Fatalf("a sentence reason lost its words: %v", rows[2])
	}
}
