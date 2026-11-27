//go:build integration

package store

import (
	"context"
	"sync"
	"testing"
)

// llm-04: attribution rows add up in sage.llm_usage and stay apart from
// fleet-10's budget counters.

func addOrFail(t *testing.T, s *LLMUsageStore, ctx context.Context, rows ...LLMAttributionRow) {
	t.Helper()
	if err := s.AddAttribution(ctx, rows); err != nil {
		t.Fatalf("add attribution: %v", err)
	}
}

func costOf(v float64) *float64 { return &v }

// Happy path: two adds sum every counter; a NULL cost on either side
// keeps the row's cost NULL; an estimate marks the row.
func TestLLMAttributionDB_AddsUp(t *testing.T) {
	s, ctx := llmUsageStore(t)
	priced := validAttribution()
	priced.CostUSD = costOf(0.0123)
	addOrFail(t, s, ctx, priced)
	addOrFail(t, s, ctx, priced)
	unpriced := validAttribution()
	unpriced.Purpose = "explain"
	unpriced.CostUSD = costOf(1)
	addOrFail(t, s, ctx, unpriced)
	unpriced.CostUSD = nil
	unpriced.Source = LLMSourceEstimate
	addOrFail(t, s, ctx, unpriced)
	rows, err := s.Attribution(ctx, attrDay)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := map[string]LLMAttributionRow{}
	for _, r := range rows {
		if r.DatabaseKey == "id:7" {
			got[r.Purpose] = r
		}
	}
	b := got["briefing"]
	if b.PromptTokens != 20 || b.CompletionTokens != 10 || b.Calls != 2 ||
		b.CostUSD == nil || *b.CostUSD != 0.0246 || b.Source != LLMSourceProvider {
		t.Fatalf("briefing = %+v, want doubled counters and cost 0.0246", b)
	}
	e := got["explain"]
	if e.Calls != 2 || e.CostUSD != nil || e.Source != LLMSourceEstimate {
		t.Fatalf("explain = %+v, want 2 calls, NULL cost, source estimate", e)
	}
}

// Attribution rows never touch the budget: Load ignores them, Clear (an
// admin budget reset) keeps them, and a budget flush leaves them as is.
func TestLLMAttributionDB_SeparateFromBudget(t *testing.T) {
	s, ctx := llmUsageStore(t)
	addOrFail(t, s, ctx, validAttribution())
	if err := s.Flush(ctx, llmUsageTestDay, []LLMUsageRow{
		{DatabaseKey: "id:7", Role: "general", Tokens: 900}}); err != nil {
		t.Fatalf("budget flush: %v", err)
	}
	loaded, err := s.Load(ctx, llmUsageTestDay)
	if err != nil || len(loaded) != 1 || loaded[0].Tokens != 900 {
		t.Fatalf("budget load = %+v (%v), want only the 900-token counter", loaded, err)
	}
	if err := s.Clear(ctx, llmUsageTestDay); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if loaded, _ := s.Load(ctx, llmUsageTestDay); len(loaded) != 0 {
		t.Fatalf("budget rows after clear = %+v", loaded)
	}
	rows, err := s.Attribution(ctx, attrDay)
	if err != nil || len(rows) != 1 || rows[0].Calls != 1 {
		t.Fatalf("attribution after a budget reset = %+v (%v), want it kept", rows, err)
	}
}

// The usage read never returns fleet-10's budget counters (purpose
// '_total'), even when they share the day, key and role.
func TestLLMAttributionDB_ReadSkipsBudgetCounters(t *testing.T) {
	s, ctx := llmUsageStore(t)
	addOrFail(t, s, ctx, validAttribution())
	if err := s.Flush(ctx, attrDay, []LLMUsageRow{
		{DatabaseKey: "id:7", Role: "general", Tokens: 900},
		{DatabaseKey: "process", Role: "shared", Tokens: 900}}); err != nil {
		t.Fatalf("budget flush: %v", err)
	}
	rows, err := s.Attribution(ctx, attrDay)
	if err != nil || len(rows) != 1 || rows[0].Purpose != "briefing" || rows[0].Calls != 1 {
		t.Fatalf("attribution = %+v (%v), want only the briefing row", rows, err)
	}
}

// Boundary: Attribution reads days from since on; PruneAttribution drops
// the days before its cutoff and the budget Prune leaves attribution.
func TestLLMAttributionDB_ReadAndPruneBoundaries(t *testing.T) {
	s, ctx := llmUsageStore(t)
	prev := validAttribution()
	prev.Day = attrDay.AddDate(0, 0, -1)
	addOrFail(t, s, ctx, prev, validAttribution())
	if rows, _ := s.Attribution(ctx, attrDay); len(rows) != 1 {
		t.Fatalf("since today read %d rows, want 1", len(rows))
	}
	if _, err := s.Prune(ctx, attrDay.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("budget prune: %v", err)
	}
	if rows, _ := s.Attribution(ctx, prev.Day); len(rows) != 2 {
		t.Fatalf("budget prune removed attribution: %d rows left, want 2", len(rows))
	}
	n, err := s.PruneAttribution(ctx, attrDay)
	if err != nil || n != 1 {
		t.Fatalf("prune attribution = %d (%v), want 1 row before the cutoff", n, err)
	}
	if rows, _ := s.Attribution(ctx, prev.Day); len(rows) != 1 || !rows[0].Day.Equal(attrDay) {
		t.Fatalf("after prune rows = %+v, want only the cutoff day", rows)
	}
}

// Concurrency: 16 writers adding the same key total exactly.
func TestLLMAttributionDB_ConcurrentAddsTotalExactly(t *testing.T) {
	s, ctx := llmUsageStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				row := []LLMAttributionRow{validAttribution()}
				if err := s.AddAttribution(ctx, row); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent add: %v", err)
	}
	rows, err := s.Attribution(ctx, attrDay)
	if err != nil || len(rows) != 1 || rows[0].Calls != 160 || rows[0].PromptTokens != 1600 {
		t.Fatalf("rows = %+v (%v), want one row of 160 calls", rows, err)
	}
}
