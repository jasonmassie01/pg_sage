package analyzer

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// dispatchRewriteFindings
// ---------------------------------------------------------------------------

func TestDispatchRewriteFindings_SkipsNonQueryTuning(t *testing.T) {
	d := &mockDispatcher{}
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	a.WithDispatcher(d)
	a.WithDatabaseName("testdb")

	findings := []Finding{
		{
			Category: "index_health",
			Severity: "warning",
			Title:    "unused index",
			Detail: map[string]any{
				"suggested_rewrite":  "SELECT 1",
				"rewrite_rationale": "test",
			},
		},
	}

	a.dispatchRewriteFindings(context.Background(), findings)

	if d.count() != 0 {
		t.Errorf("expected 0 dispatched events for non-query_tuning, got %d",
			d.count())
	}
}

func TestDispatchRewriteFindings_SkipsNoRewrite(t *testing.T) {
	d := &mockDispatcher{}
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	a.WithDispatcher(d)
	a.WithDatabaseName("testdb")

	findings := []Finding{
		{
			Category: "query_tuning",
			Severity: "warning",
			Title:    "slow query hint",
			Detail:   map[string]any{"hint_directive": "Set(work_mem '64MB')"},
		},
	}

	a.dispatchRewriteFindings(context.Background(), findings)

	if d.count() != 0 {
		t.Errorf(
			"expected 0 dispatched events when no rewrite, got %d",
			d.count())
	}
}

func TestDispatchRewriteFindings_DispatchesRewrite(t *testing.T) {
	d := &mockDispatcher{}
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	a.WithDispatcher(d)
	a.WithDatabaseName("testdb")

	findings := []Finding{
		{
			Category: "query_tuning",
			Severity: "warning",
			Title:    "Per-query tuning: force index scan",
			Detail: map[string]any{
				"query":             "SELECT * FROM orders WHERE id IN (SELECT oid FROM items)",
				"suggested_rewrite": "SELECT o.* FROM orders o JOIN items i ON o.id = i.order_id",
				"rewrite_rationale": "replace IN subquery with JOIN",
			},
		},
	}

	a.dispatchRewriteFindings(context.Background(), findings)

	if d.count() != 1 {
		t.Fatalf("expected 1 dispatched event, got %d", d.count())
	}

	d.mu.Lock()
	evt := d.events[0]
	d.mu.Unlock()

	if evt.Type != "query_rewrite_suggested" {
		t.Errorf("event Type = %q, want query_rewrite_suggested", evt.Type)
	}
	if evt.Data["rewrite"] != "SELECT o.* FROM orders o JOIN items i ON o.id = i.order_id" {
		t.Errorf("event Data[rewrite] = %v", evt.Data["rewrite"])
	}
	if evt.Data["database"] != "testdb" {
		t.Errorf("event Data[database] = %v, want testdb", evt.Data["database"])
	}
}

func TestDispatchRewriteFindings_NilDispatcher(t *testing.T) {
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	// Should not panic with nil dispatcher.
	a.dispatchRewriteFindings(context.Background(), []Finding{
		{
			Category: "query_tuning",
			Severity: "warning",
			Title:    "rewrite test",
			Detail: map[string]any{
				"query":             "SELECT 1",
				"suggested_rewrite": "SELECT 2",
				"rewrite_rationale": "just because",
			},
		},
	})
}

func TestDispatchRewriteFindings_MixedFindings(t *testing.T) {
	d := &mockDispatcher{}
	a := New(nil, coverageTestConfig(), nil, nil, nil, nil, nil, noopLog)
	a.WithDispatcher(d)
	a.WithDatabaseName("testdb")

	findings := []Finding{
		// Non-query_tuning: skipped
		{Category: "index_health", Severity: "critical", Title: "dup index",
			Detail: map[string]any{"suggested_rewrite": "DROP INDEX ..."}},
		// query_tuning but no rewrite: skipped
		{Category: "query_tuning", Severity: "warning", Title: "hint only",
			Detail: map[string]any{"hint_directive": "SeqScan(t1)"}},
		// query_tuning with empty rewrite string: skipped
		{Category: "query_tuning", Severity: "warning", Title: "empty rewrite",
			Detail: map[string]any{"suggested_rewrite": ""}},
		// query_tuning with rewrite: dispatched
		{Category: "query_tuning", Severity: "warning", Title: "real rewrite",
			Detail: map[string]any{
				"query":             "SELECT * FROM t",
				"suggested_rewrite": "SELECT id FROM t",
				"rewrite_rationale": "reduce columns",
			}},
		// Another query_tuning with rewrite: dispatched
		{Category: "query_tuning", Severity: "warning", Title: "second rewrite",
			Detail: map[string]any{
				"query":             "SELECT * FROM t2",
				"suggested_rewrite": "SELECT id FROM t2",
				"rewrite_rationale": "reduce columns again",
			}},
	}

	a.dispatchRewriteFindings(context.Background(), findings)

	if d.count() != 2 {
		t.Errorf("expected 2 dispatched events, got %d", d.count())
	}
}
