//go:build integration

package store

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

// fleet-10: sage.llm_usage keeps today's LLM token usage across restarts.
// A flush is idempotent and monotonic; only the asked day is loaded.

// llmUsageTestDay is far from today so the test never meets live rows.
var llmUsageTestDay = time.Date(2031, 3, 14, 0, 0, 0, 0, time.UTC)

func llmUsageStore(t *testing.T) (*LLMUsageStore, context.Context) {
	t.Helper()
	pool, ctx := requireDB(t)
	clear := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.llm_usage WHERE day BETWEEN $1::date - 1 AND $1::date + 1`,
			llmUsageTestDay)
	}
	clear()
	t.Cleanup(clear)
	return NewLLMUsageStore(pool), ctx
}

func sortedUsage(rows []LLMUsageRow) []LLMUsageRow {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].DatabaseKey != rows[j].DatabaseKey {
			return rows[i].DatabaseKey < rows[j].DatabaseKey
		}
		return rows[i].Role < rows[j].Role
	})
	return rows
}

func TestLLMUsageDB_FlushThenLoadRoundTrips(t *testing.T) {
	s, ctx := llmUsageStore(t)
	in := []LLMUsageRow{
		{DatabaseKey: "id:7", Role: "fleet", Tokens: 400},
		{DatabaseKey: "id:7", Role: "general", Tokens: 250},
		{DatabaseKey: "process", Role: "shared", Tokens: 900},
	}
	if err := s.Flush(ctx, llmUsageTestDay, in); err != nil {
		t.Fatalf("flush: %v", err)
	}
	out, err := s.Load(ctx, llmUsageTestDay)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	out = sortedUsage(out)
	if len(out) != 3 {
		t.Fatalf("loaded %d rows, want 3: %+v", len(out), out)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("row %d = %+v, want %+v", i, out[i], in[i])
		}
	}
}

func TestLLMUsageDB_FlushIsMonotonic(t *testing.T) {
	s, ctx := llmUsageStore(t)
	row := func(tokens int64) []LLMUsageRow {
		return []LLMUsageRow{{DatabaseKey: "name:orders", Role: "general", Tokens: tokens}}
	}
	// Load after every flush: a lower flush must not lower the row even
	// for a moment, and repeating a count changes nothing.
	for _, tokens := range []int64{100, 50, 100} {
		if err := s.Flush(ctx, llmUsageTestDay, row(tokens)); err != nil {
			t.Fatalf("flush %d: %v", tokens, err)
		}
		out, err := s.Load(ctx, llmUsageTestDay)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || out[0].Tokens != 100 {
			t.Fatalf("after flushing %d: %+v, want one row of 100", tokens, out)
		}
	}
	if err := s.Flush(ctx, llmUsageTestDay, row(160)); err != nil {
		t.Fatal(err)
	}
	out, err := s.Load(ctx, llmUsageTestDay)
	if err != nil || len(out) != 1 || out[0].Tokens != 160 {
		t.Fatalf("after flushing 160: %+v (%v), want 160", out, err)
	}
}

func TestLLMUsageDB_YesterdayIsNotLoaded(t *testing.T) {
	s, ctx := llmUsageStore(t)
	yesterday := llmUsageTestDay.Add(-24 * time.Hour)
	if err := s.Flush(ctx, yesterday, []LLMUsageRow{
		{DatabaseKey: "name:orders", Role: "fleet", Tokens: 777}}); err != nil {
		t.Fatal(err)
	}
	out, err := s.Load(ctx, llmUsageTestDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("today's load returned %+v, want nothing (only yesterday has rows)", out)
	}
	out, _ = s.Load(ctx, yesterday)
	if len(out) != 1 || out[0].Tokens != 777 {
		t.Fatalf("yesterday's load = %+v, want 777", out)
	}
}

func TestLLMUsageDB_DuplicateRowsInOneFlushKeepTheMax(t *testing.T) {
	s, ctx := llmUsageStore(t)
	err := s.Flush(ctx, llmUsageTestDay, []LLMUsageRow{
		{DatabaseKey: "name:a", Role: "general", Tokens: 30},
		{DatabaseKey: "name:a", Role: "general", Tokens: 90},
	})
	if err != nil {
		t.Fatalf("flush with a repeated key: %v", err)
	}
	out, _ := s.Load(ctx, llmUsageTestDay)
	if len(out) != 1 || out[0].Tokens != 90 {
		t.Fatalf("rows = %+v, want one row of 90", out)
	}
}

func TestLLMUsageDB_ClearRemovesOnlyThatDay(t *testing.T) {
	s, ctx := llmUsageStore(t)
	yesterday := llmUsageTestDay.Add(-24 * time.Hour)
	row := []LLMUsageRow{{DatabaseKey: "name:a", Role: "fleet", Tokens: 5}}
	if err := s.Flush(ctx, llmUsageTestDay, row); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx, yesterday, row); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(ctx, llmUsageTestDay); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if out, _ := s.Load(ctx, llmUsageTestDay); len(out) != 0 {
		t.Fatalf("cleared day still has %+v", out)
	}
	if out, _ := s.Load(ctx, yesterday); len(out) != 1 {
		t.Fatalf("clear removed another day's rows: %+v", out)
	}
}

func TestLLMUsageDB_PruneDropsOldDays(t *testing.T) {
	s, ctx := llmUsageStore(t)
	yesterday := llmUsageTestDay.Add(-24 * time.Hour)
	row := []LLMUsageRow{{DatabaseKey: "name:a", Role: "fleet", Tokens: 5}}
	for _, day := range []time.Time{yesterday, llmUsageTestDay} {
		if err := s.Flush(ctx, day, row); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Prune(ctx, llmUsageTestDay)
	if err != nil || n != 1 {
		t.Fatalf("prune = (%d, %v), want 1 row", n, err)
	}
	if out, _ := s.Load(ctx, llmUsageTestDay); len(out) != 1 {
		t.Fatalf("prune removed the cutoff day: %+v", out)
	}
}

// The database rejects a negative count even past the Go validation.
func TestLLMUsageDB_TableRejectsNegativeTokens(t *testing.T) {
	pool, ctx := requireDB(t)
	_, err := pool.Exec(ctx, `INSERT INTO sage.llm_usage (day, database_key, role, tokens)
		VALUES ($1, 'name:x', 'fleet', -1)`, llmUsageTestDay)
	if err == nil {
		t.Fatal("a negative token count must violate the table's check")
	}
}

func TestLLMUsageDB_ConcurrentFlushesKeepTheMax(t *testing.T) {
	s, ctx := llmUsageStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func(tokens int64) {
			defer wg.Done()
			errs <- s.Flush(ctx, llmUsageTestDay, []LLMUsageRow{
				{DatabaseKey: "name:hot", Role: "fleet", Tokens: tokens * 10}})
		}(int64(i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent flush: %v", err)
		}
	}
	out, _ := s.Load(ctx, llmUsageTestDay)
	if len(out) != 1 || out[0].Tokens != 200 {
		t.Fatalf("rows = %+v, want one row of 200", out)
	}
}
