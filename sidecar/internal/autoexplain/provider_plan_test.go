package autoexplain

import (
	"context"
	"github.com/pg-sage/sidecar/internal/schema"
	"math"
	"sync"
	"testing"
	"time"
)

func TestProviderPlanReplayPreservesProvenanceAndSeparatesScopes(t *testing.T) {
	pool := acquireTestPool(t)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	qid := int64(87654321)
	cleanExplainRows(t, pool, qid)
	t.Cleanup(func() { cleanExplainRows(t, pool, qid) })
	off := false
	p := ProviderPlan{ObservedPlan: ObservedPlan{QueryID: qid, Query: "SELECT 1", JSON: []byte(`[{"Plan":{"Node Type":"Result","Total Cost":1}}]`), TotalCost: 1, ExecutionMS: 5, CapturedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)}, ScopeHash: "scope-a", ComputeID: "compute-a", EventID: "event-a", ReceivedAt: time.Now().UTC().Truncate(time.Microsecond), AnalyzeEnabled: &off, DurationSource: "provider_duration_ms"}
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := StoreProviderPlan(ctx, pool, p); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var count int
	var captured, received time.Time
	var analyze, buffers *bool
	var comparison *string
	var source, durationSource string
	err := pool.QueryRow(ctx, `SELECT count(*),min(captured_at),min(received_at),bool_or(analyze_enabled),bool_or(buffers_enabled),min(comparison_key),min(source),min(duration_source) FROM sage.explain_cache WHERE queryid=$1`, qid).Scan(&count, &captured, &received, &analyze, &buffers, &comparison, &source, &durationSource)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || !captured.Equal(p.CapturedAt) || !received.Equal(p.ReceivedAt) || analyze == nil || *analyze || buffers != nil || comparison != nil || source != LakebasePlanSource || durationSource != p.DurationSource {
		t.Fatalf("provenance lost: count=%d captured=%v received=%v flags=%v/%v source=%s duration=%s", count, captured, received, analyze, buffers, source, durationSource)
	}
	p.ScopeHash = "scope-b"
	if err = StoreProviderPlan(ctx, pool, p); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM sage.explain_cache WHERE queryid=$1`, qid).Scan(&count); err != nil || count != 2 {
		t.Fatalf("resource identities collapsed: %d %v", count, err)
	}
}
func TestProviderPlanInvalidNeverWrites(t *testing.T) {
	pool := acquireTestPool(t)
	p := ProviderPlan{ObservedPlan: ObservedPlan{QueryID: 1, Query: "SELECT 1", JSON: []byte(`[{"Plan":{"Node Type":"Result","Total Cost":1}}]`), CapturedAt: time.Now()}, ScopeHash: "a", ComputeID: "c", EventID: "e", ReceivedAt: time.Now()}
	for _, mutate := range []func(*ProviderPlan){func(p *ProviderPlan) { p.EventID = "" }, func(p *ProviderPlan) { p.JSON = []byte(`{`) }, func(p *ProviderPlan) { p.JSON = []byte(`[]`) }, func(p *ProviderPlan) { p.TotalCost = math.NaN() }, func(p *ProviderPlan) { p.ExecutionMS = math.Inf(1) }, func(p *ProviderPlan) { p.Query = "/* pg_sage */ SELECT 1" }} {
		candidate := p
		mutate(&candidate)
		if err := StoreProviderPlan(t.Context(), pool, candidate); err == nil {
			t.Fatal("invalid provider plan accepted")
		}
	}
}
