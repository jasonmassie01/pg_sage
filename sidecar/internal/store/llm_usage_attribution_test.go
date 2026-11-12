package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-04: attribution rows (purpose, model, split, calls, cost) share
// sage.llm_usage with fleet-10's budget counters (purpose '_total').
//
// No concurrent-access tests here: the conversion and validation are pure;
// the database tests cover the concurrent upsert.

var attrDay = time.Date(2031, 3, 14, 0, 0, 0, 0, time.UTC)

func validAttribution() LLMAttributionRow {
	return LLMAttributionRow{Day: attrDay, DatabaseKey: "id:7", Role: "general",
		Purpose: "briefing", Model: "m", PromptTokens: 10, CompletionTokens: 5,
		Calls: 1, Source: LLMSourceProvider}
}

// Invalid input is refused before any SQL, with the field named.
func TestLLMAttributionValidation(t *testing.T) {
	negative := -0.01
	cases := map[string]struct {
		edit func(*LLMAttributionRow)
		want string
	}{
		"empty key":      {func(r *LLMAttributionRow) { r.DatabaseKey = "" }, "database key"},
		"budget role":    {func(r *LLMAttributionRow) { r.Role = "shared" }, "role"},
		"budget purpose": {func(r *LLMAttributionRow) { r.Purpose = "_total" }, "purpose"},
		"bad purpose":    {func(r *LLMAttributionRow) { r.Purpose = "Has Space" }, "purpose"},
		"negative calls": {func(r *LLMAttributionRow) { r.Calls = -1 }, "negative"},
		"negative tokens": {func(r *LLMAttributionRow) { r.ReasoningTokens = -1 },
			"negative"},
		"negative cost": {func(r *LLMAttributionRow) { r.CostUSD = &negative }, "cost"},
		"bad source":    {func(r *LLMAttributionRow) { r.Source = "guess" }, "source"},
		"zero day":      {func(r *LLMAttributionRow) { r.Day = time.Time{} }, "day"},
	}
	for name, c := range cases {
		row := validAttribution()
		c.edit(&row)
		err := NewLLMUsageStore(nil).AddAttribution(context.Background(),
			[]LLMAttributionRow{row})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, c.want)
		}
	}
}

// Nil/empty: no rows is a no-op even without a pool; valid rows without a
// pool say the ledger has no control database.
func TestLLMAttributionNoRowsNoPool(t *testing.T) {
	s := NewLLMUsageStore(nil)
	if err := s.AddAttribution(context.Background(), nil); err != nil {
		t.Fatalf("empty add: %v", err)
	}
	err := s.AddAttribution(context.Background(), []LLMAttributionRow{validAttribution()})
	if err == nil || !strings.Contains(err.Error(), "control database") {
		t.Fatalf("add without a pool = %v, want a control database error", err)
	}
	if _, err := s.Attribution(context.Background(), attrDay); err == nil {
		t.Fatal("read without a pool returned nil")
	}
}

// The accumulator's deltas become rows under each database's ledger key;
// two names behind one key are summed, an unpriced delta has a nil cost
// and an estimated one is source estimate.
func TestAttributionRowsConvertsDeltas(t *testing.T) {
	k := func(db, purpose string) llm.UsageKey {
		return llm.UsageKey{Day: attrDay, Database: db, Role: "general",
			Purpose: purpose, Model: "m"}
	}
	deltas := map[llm.UsageKey]llm.UsageCounts{
		k("old", "briefing"): {PromptTokens: 10, CompletionTokens: 2, Calls: 1,
			CostUSD: 0.5, Priced: true},
		k("new", "briefing"): {PromptTokens: 5, CompletionTokens: 1, Calls: 1,
			Failures: 1, CostUSD: 0.25, Priced: true},
		k("new", "explain"): {PromptTokens: 7, Calls: 1, Estimated: true},
	}
	keyOf := func(db string) string { return "id:7" } // a rename: both names, one id
	rows := AttributionRows(deltas, keyOf)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (briefing merged)", rows)
	}
	byPurpose := map[string]LLMAttributionRow{}
	for _, r := range rows {
		byPurpose[r.Purpose] = r
	}
	b := byPurpose["briefing"]
	if b.DatabaseKey != "id:7" || b.PromptTokens != 15 || b.CompletionTokens != 3 ||
		b.Calls != 2 || b.Failures != 1 || b.CostUSD == nil || *b.CostUSD != 0.75 ||
		b.Source != LLMSourceProvider {
		t.Fatalf("briefing = %+v, want the two names summed at cost 0.75", b)
	}
	e := byPurpose["explain"]
	if e.CostUSD != nil || e.Source != LLMSourceEstimate || e.PromptTokens != 7 {
		t.Fatalf("explain = %+v, want nil cost, source estimate", e)
	}
	if got := AttributionRows(nil, keyOf); len(got) != 0 {
		t.Fatalf("no deltas gave %d rows", len(got))
	}
}

// Merging a priced and an unpriced delta of one key leaves the cost
// unknown: a partial sum would understate the spend.
func TestAttributionRowsPartialPriceIsNull(t *testing.T) {
	deltas := map[llm.UsageKey]llm.UsageCounts{
		{Day: attrDay, Database: "a", Role: "general", Purpose: "facts", Model: "m"}: {
			Calls: 1, CostUSD: 1, Priced: true},
		{Day: attrDay, Database: "b", Role: "general", Purpose: "facts", Model: "m"}: {
			Calls: 1},
	}
	rows := AttributionRows(deltas, func(string) string { return "process" })
	if len(rows) != 1 || rows[0].CostUSD != nil || rows[0].Calls != 2 {
		t.Fatalf("rows = %+v, want one row of 2 calls with a NULL cost", rows)
	}
}
