package analyzer

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pg-sage/sidecar/internal/config"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// history.store: meta. The regression baseline reads the 'queries'
// snapshots from the store; the footprint guard keeps measuring the
// monitored database's sage schema (where history no longer grows); the
// self-budget's storage adds this database's share of the store.

func writeQuerySnapshots(t *testing.T, p *histfixture.Pair, start time.Time, n int) {
	t.Helper()
	w := snapstore.NewWriter()
	for i := 0; i < n; i++ {
		doc, err := json.Marshal([]map[string]any{
			{"queryid": 501, "calls": 10 * (i + 1), "mean_exec_time": 2.0 + float64(i)},
			{"queryid": 502, "calls": 3 * (i + 1), "mean_exec_time": 40.0},
		})
		if err != nil {
			t.Fatal(err)
		}
		at := start.Add(time.Duration(i) * time.Hour)
		if err := w.Persist(context.Background(), p.Monitored, at,
			[]snapstore.Row{{Category: "queries", Data: doc}}); err != nil {
			t.Fatalf("persist queries snapshot: %v", err)
		}
	}
}

func TestRegressionBaselineIdenticalInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	start := time.Now().Add(-48 * time.Hour)
	got := map[histstore.Mode]map[int64]float64{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		p.Noise(t, start.Add(90*time.Minute), 501, 502)
		writeQuerySnapshots(t, p, start, 12)
		c := phase2Config()
		c.Analyzer.RegressionLookbackDays = 7
		a := New(p.Monitored, c, nil, nil, nil, nil, nil, noopLog)
		got[mode] = a.buildHistoricalAverages(context.Background())
	}
	mon, meta := got[histstore.ModeMonitored], got[histstore.ModeMeta]
	if len(mon) != 2 || mon[502] != 40 {
		t.Fatalf("fixture baseline = %v, want queries 501 and 502 (502 at 40 ms)", mon)
	}
	if len(meta) != len(mon) || meta[501] != mon[501] || meta[502] != mon[502] {
		t.Fatalf("baselines differ: monitored %v, meta %v (the other database's 10 s "+
			"means must stay out)", mon, meta)
	}
}

func TestFootprintExcludesHistoryKeptInTheStore(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	a := New(p.Monitored, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	ctx := context.Background()
	before, err := a.measureSageFootprint(ctx)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	writeQuerySnapshots(t, p, time.Now().Add(-10*time.Hour), 10)
	after, err := a.measureSageFootprint(ctx)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if after.total != before.total {
		t.Fatalf("history written in meta mode grew the monitored sage schema: %d -> %d",
			before.total, after.total)
	}
	if _, meta := p.Count(t, "snapshots"); meta != 10 {
		t.Fatalf("store holds %d snapshots, want 10", meta)
	}
}

func TestFootprintFindingSaysWhereHistoryLives(t *testing.T) {
	// Above the 256 MB floor (v2.2.1), at 20% of the database.
	unit := config.MinSnapshotCapBytes
	base := ruleSageFootprint(sageFootprint{database: "app", total: 2 * unit,
		tables: []sageTableSize{{name: "sage.snapshots", bytes: unit + unit/2},
			{name: "sage.findings", bytes: unit / 2}}}, 10*unit, 10)
	if len(base) != 1 {
		t.Fatalf("fixture must raise the finding, got %d", len(base))
	}
	mon := annotateHistoryPlacement(cloneFindings(base), histstore.ModeMonitored)
	if _, ok := mon[0].Detail["history_store"]; ok ||
		mon[0].Recommendation != base[0].Recommendation {
		t.Fatalf("monitored mode must leave the finding as it was: %+v", mon[0])
	}
	meta := annotateHistoryPlacement(cloneFindings(base), histstore.ModeMeta)
	if meta[0].Detail["history_store"] != "meta" {
		t.Fatalf("meta mode must say where history lives: %v", meta[0].Detail)
	}
	rec := meta[0].Recommendation
	if !strings.Contains(rec, "meta database") || !strings.Contains(rec, "--cleanup") {
		t.Fatalf("meta mode advice must point at the leftover rows' cleanup: %q", rec)
	}
	if strings.Contains(strings.ToLower(rec), "pg_sage") {
		t.Fatalf("the finding must not name the sidecar (self-monitoring filter): %q", rec)
	}
	none := annotateHistoryPlacement(nil, histstore.ModeMeta)
	if len(none) != 0 {
		t.Fatalf("no finding stays no finding: %v", none)
	}
	noHistory := ruleSageFootprint(sageFootprint{database: "app", total: 2000,
		tables: []sageTableSize{{name: "sage.findings", bytes: 2000}}}, 10000, 10)
	got := annotateHistoryPlacement(noHistory, histstore.ModeMeta)
	if strings.Contains(got[0].Recommendation, "--cleanup") {
		t.Fatalf("no history left in the monitored database: no cleanup advice: %q",
			got[0].Recommendation)
	}
}

func cloneFindings(in []Finding) []Finding {
	out := make([]Finding, len(in))
	for i, f := range in {
		f.Detail = map[string]any{}
		for k, v := range in[i].Detail {
			f.Detail[k] = v
		}
		out[i] = f
	}
	return out
}

func TestSelfBudgetStorageAddsTheStoreShare(t *testing.T) {
	u := selfbudget.Usage{StorageKnown: true, StorageBytes: 100}
	if got := withHistoryStorage(u, 50, nil); !got.StorageKnown || got.StorageBytes != 150 {
		t.Fatalf("store share must be added: %+v", got)
	}
	if got := withHistoryStorage(u, 0, nil); got.StorageBytes != 100 {
		t.Fatalf("monitored mode adds nothing: %+v", got)
	}
	if got := withHistoryStorage(u, 50, errors.New("store down")); got.StorageKnown {
		t.Fatalf("an unknown store share makes storage unknown (proves nothing): %+v", got)
	}
	unknown := selfbudget.Usage{}
	if got := withHistoryStorage(unknown, 50, nil); got.StorageKnown {
		t.Fatalf("an unknown schema size stays unknown: %+v", got)
	}
}

func TestHistoryStorageBytesByPlacement(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		writeQuerySnapshots(t, p, time.Now().Add(-10*time.Hour), 10)
		a := New(p.Monitored, phase2Config(), nil, nil, nil, nil, nil, noopLog)
		b, err := a.historyStorage(ctx)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if mode == histstore.ModeMonitored && b != 0 {
			t.Fatalf("monitored mode: history is already in the schema size, got %d", b)
		}
		if mode == histstore.ModeMeta && b <= 0 {
			t.Fatalf("meta mode: the store share must be counted, got %d", b)
		}
	}
}
