package forecaster

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// forecast-04 review: the cache finding a Forecast run returns carries the
// judged day's top tables by heap_blks_read growth, and a failed evidence
// read leaves the finding standing on its ratio alone.

// No concurrent access tests: addCacheEvidence mutates only the findings
// slice its caller owns.

func TestCacheForecast_FindingNamesTheDaysTopTables(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedCacheDays(t, ctx, pool)
	cleanupSnapshots(t, pool, ctx, "tables")
	t.Cleanup(func() { cleanupSnapshots(t, pool, context.Background(), "tables") })
	day := dayStart(1)
	insertSnapshot(t, ctx, pool, day.Add(time.Hour), "tables",
		tablesDoc(map[string]int64{"hot": 10, "warm": 10}))
	insertSnapshot(t, ctx, pool, day.Add(22*time.Hour), "tables",
		tablesDoc(map[string]int64{"hot": 7010, "warm": 310}))
	// Another day's growth must not be named.
	insertSnapshot(t, ctx, pool, dayStart(2).Add(time.Hour), "tables",
		tablesDoc(map[string]int64{"hot": 0, "other": 0}))
	insertSnapshot(t, ctx, pool, dayStart(2).Add(22*time.Hour), "tables",
		tablesDoc(map[string]int64{"hot": 1, "other": 1e9}))

	f := New(pool, ForecasterConfig{LookbackDays: 30, CacheWarnThreshold: 0.95},
		func(string, string, ...any) {})
	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	var low map[string]any
	for _, fd := range findings {
		if fd.Category == "forecast_cache_pressure" &&
			fd.Detail["forecast_type"] == "cache_pressure" {
			low = fd.Detail
		}
	}
	if low == nil {
		t.Fatalf("no low finding for day N at 80%%: %+v", findings)
	}
	got := fmt.Sprint(low["top_tables_by_heap_blks_read"])
	want := fmt.Sprint([]map[string]any{
		{"table": "public.hot", "heap_blks_read": int64(7000)},
		{"table": "public.warm", "heap_blks_read": int64(300)},
	})
	if got != want {
		t.Fatalf("top tables = %s, want %s", got, want)
	}
}

// A failed evidence read (here a cancelled context) is logged per read,
// and the findings keep their ratio detail without invented evidence.
func TestAddCacheEvidence_FailedReadsLeaveTheFinding(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	var mu sync.Mutex
	var warns []string
	f := New(pool, ForecasterConfig{}, func(level, format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if level == "WARN" {
			warns = append(warns, fmt.Sprintf(format, args...))
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	findings := []analyzer.Finding{{Category: "forecast_cache_pressure",
		Detail: map[string]any{"day": dayStart(1), "day_hit_ratio": 0.8}}}
	f.addCacheEvidence(ctx, findings)
	d := findings[0].Detail
	if _, ok := d["top_tables_by_heap_blks_read"]; ok {
		t.Errorf("a failed table read attached tables: %+v", d)
	}
	if _, ok := d["shared_buffers"]; ok {
		t.Errorf("a failed shared_buffers read attached a value: %+v", d)
	}
	if d["day_hit_ratio"] != 0.8 || len(d) != 2 {
		t.Errorf("the ratio detail changed: %+v", d)
	}
	joined := strings.Join(warns, "\n")
	if len(warns) != 2 || !strings.Contains(joined, "cache top tables") ||
		!strings.Contains(joined, "shared_buffers") {
		t.Fatalf("warnings = %q, want one per failed read", warns)
	}
	// No day in the detail: nothing is read and nothing is logged.
	warns = nil
	bare := []analyzer.Finding{{Detail: map[string]any{}}}
	f.addCacheEvidence(context.Background(), bare)
	if len(bare[0].Detail) != 0 || len(warns) != 0 {
		t.Fatalf("a finding without a day got %+v, warnings %q", bare[0].Detail, warns)
	}
	f.addCacheEvidence(context.Background(), nil) // no findings: no read, no panic
}
