package forecaster

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// history.store: meta. The forecaster's daily aggregates (system metrics,
// query volume, sequence use) must be identical in both placements, with
// another database's snapshots in the store.

func persistDay(t *testing.T, p *histfixture.Pair, w *snapstore.Writer, at time.Time, i int) {
	t.Helper()
	system, _ := json.Marshal(map[string]any{"db_size_bytes": 1_000_000 * (i + 1),
		"active_backends": 3 + i%4, "total_backends": 10, "max_connections": 100,
		"cache_hit_ratio": 0.95, "total_checkpoints": 100 + i})
	queries, _ := json.Marshal([]map[string]any{{"queryid": 31, "calls": 1000 * (i + 1)},
		{"queryid": 32, "calls": 10 * (i + 1)}})
	seqs, _ := json.Marshal([]map[string]any{{"schemaname": "public",
		"sequencename": "s1", "pct_used": 10 + float64(i), "max_value": 1 << 40}})
	rows := []snapstore.Row{{Category: "queries", Data: queries},
		{Category: "sequences", Data: seqs}, {Category: "system", Data: system}}
	if err := w.Persist(context.Background(), p.Monitored, at, rows); err != nil {
		t.Fatalf("persist %d: %v", i, err)
	}
}

func TestDailyAggregatesIdenticalInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	start := time.Now().Add(-6 * 24 * time.Hour)
	got := map[histstore.Mode]string{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		w := snapstore.NewWriter()
		for i := 0; i < 12; i++ {
			at := start.Add(time.Duration(i) * 12 * time.Hour)
			persistDay(t, p, w, at, i)
			p.Noise(t, at.Add(time.Minute), 31, 32)
		}
		sys, err := QueryDailySystemAggs(ctx, p.Monitored, 14)
		if err != nil {
			t.Fatalf("%s system aggs: %v", mode, err)
		}
		q, err := QueryDailyQueryAggs(ctx, p.Monitored, 14)
		if err != nil {
			t.Fatalf("%s query aggs: %v", mode, err)
		}
		s, err := QueryDailySeqAggs(ctx, p.Monitored, 14)
		if err != nil {
			t.Fatalf("%s seq aggs: %v", mode, err)
		}
		if len(sys) == 0 || len(q) == 0 || len(s) == 0 {
			t.Fatalf("%s: empty aggregates (system %d, queries %d, sequences %d)", mode,
				len(sys), len(q), len(s))
		}
		for _, a := range sys {
			if a.MaxConnections != 100 {
				t.Fatalf("%s: max_connections %v: another database's snapshot leaked in",
					mode, a.MaxConnections)
			}
		}
		for _, a := range s {
			if a.SeqName != "public.s1" {
				t.Fatalf("%s: sequence %q: another database's snapshot leaked in", mode,
					a.SeqName)
			}
		}
		got[mode] = fmt.Sprintf("%+v|%+v|%+v", sys, q, s)
	}
	if got[histstore.ModeMonitored] != got[histstore.ModeMeta] {
		t.Fatalf("aggregates differ:\nmonitored %s\nmeta      %s",
			got[histstore.ModeMonitored], got[histstore.ModeMeta])
	}
}

func TestForecasterCacheIsPerPlacement(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	p.Switch(t, histstore.ModeMeta)
	w := snapstore.NewWriter()
	start := time.Now().Add(-3 * 24 * time.Hour)
	for i := 0; i < 6; i++ {
		persistDay(t, p, w, start.Add(time.Duration(i)*12*time.Hour), i)
	}
	f := New(p.Monitored, ForecasterConfig{LookbackDays: 7}, func(string, string, ...any) {})
	first, err := f.dailyQueryAggs(ctx)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	again, err := f.dailyQueryAggs(ctx)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(first) == 0 || !reflect.DeepEqual(first, again) {
		t.Fatalf("cached read differs: %+v vs %+v", first, again)
	}
}
