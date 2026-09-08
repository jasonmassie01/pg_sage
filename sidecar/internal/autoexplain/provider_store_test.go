package autoexplain

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/schema"
)

func TestObservedPlanStoreDeduplicatesConcurrentDelivery(t *testing.T) {
	pool := acquireTestPool(t)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	p := ObservedPlan{QueryID: 76543210, Query: "SELECT 1",
		CapturedAt: time.Now().UTC().Truncate(time.Microsecond),
		JSON:       []byte(`[{"Plan":{"Total Cost":0.01,"Actual Rows":1}}]`),
		TotalCost:  0.01, ExecutionMS: 5.5}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := StoreObservedPlan(ctx, pool, p); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var count int
	var source, query string
	var duration, cost float64
	var captured time.Time
	err := pool.QueryRow(ctx, `SELECT count(*), min(source), min(query_text), min(execution_time),
		min(total_cost), min(captured_at) FROM sage.explain_cache WHERE queryid=$1`, p.QueryID).
		Scan(&count, &source, &query, &duration, &cost, &captured)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || source != "auto_explain_log" || query != p.Query || duration != 5.5 ||
		cost != 0.01 || !captured.Equal(p.CapturedAt) {
		t.Fatalf("stored %d %s %s %g %g %v", count, source, query, duration, cost, captured)
	}
}

func TestObservedPlanStoreRejectsMissingIdentityAndPropagatesFailure(t *testing.T) {
	if err := StoreObservedPlan(context.Background(), nil, ObservedPlan{}); err == nil {
		t.Fatal("accepted unavailable store and plan identity")
	}
	pool := acquireTestPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := ObservedPlan{QueryID: 7, CapturedAt: time.Now(), JSON: []byte(`[]`)}
	if err := StoreObservedPlan(ctx, pool, p); err == nil {
		t.Fatal("ignored cancellation")
	}
}
