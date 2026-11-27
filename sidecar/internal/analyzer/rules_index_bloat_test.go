package analyzer

import (
	"math"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// rules-13: the index_bloat rule judges the collector's catalog-statistics
// btree bloat estimate against analyzer.index_bloat_threshold_pct: info
// from the threshold, warning from twice the threshold, and the fix is
// REINDEX INDEX CONCURRENTLY under the reindex_concurrently contract.
//
// No concurrent access tests: the rule is a pure function of the snapshot
// and the configuration and keeps no state.

func bloatRow(schema, index string, pct float64) collector.IndexBloat {
	return collector.IndexBloat{SchemaName: schema, RelName: "orders", IndexRelName: index,
		IndexRelID: 4242, RelPages: 20000, EstimatedPages: 12000,
		IndexBytes: 20000 * 8192, BloatBytes: 8000 * 8192, BloatPct: pct,
		Fillfactor: 90, Reltuples: 3.5e6}
}

func bloatConfig(threshold int) *config.Config {
	return &config.Config{Analyzer: config.AnalyzerConfig{IndexBloatThresholdPct: threshold}}
}

func bloatSnapshot(rows ...collector.IndexBloat) *collector.Snapshot {
	if rows == nil {
		rows = []collector.IndexBloat{}
	}
	return &collector.Snapshot{IndexBloat: rows}
}

// Boundary: threshold - 1, the threshold and twice the threshold give no
// finding, info and warning.
func TestIndexBloat_ThresholdBoundaries(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{29, ""}, {29.99, ""}, {30, "info"}, {59.99, "info"}, {60, "warning"}, {97.5, "warning"},
	}
	for _, tc := range cases {
		got := ruleIndexBloat(bloatSnapshot(bloatRow("public", "orders_idx", tc.pct)), nil,
			bloatConfig(30), nil)
		switch {
		case tc.want == "" && len(got) != 0:
			t.Errorf("%.2f%%: findings %+v, want none", tc.pct, got)
		case tc.want != "" && (len(got) != 1 || got[0].Severity != tc.want):
			t.Errorf("%.2f%%: findings %+v, want one %s", tc.pct, got, tc.want)
		}
	}
}

// Happy path: the finding names the index, carries the estimate and
// recommends an online rebuild with quoted identifiers.
func TestIndexBloat_FindingCarriesEstimateAndReindex(t *testing.T) {
	got := ruleIndexBloat(bloatSnapshot(bloatRow("App", "Orders Idx", 40)), nil,
		bloatConfig(30), nil)
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one", got)
	}
	f := got[0]
	if f.Category != "index_bloat" || f.ObjectType != "index" ||
		f.ObjectIdentifier != "App.Orders Idx" || f.ActionRisk != "moderate" {
		t.Fatalf("finding = %+v, want index_bloat on index App.Orders Idx, moderate", f)
	}
	if want := `REINDEX INDEX CONCURRENTLY "App"."Orders Idx";`; f.RecommendedSQL != want {
		t.Fatalf("RecommendedSQL = %q, want %q", f.RecommendedSQL, want)
	}
	if f.RollbackSQL != "" {
		t.Fatalf("RollbackSQL = %q, want none (a rebuild has no undo)", f.RollbackSQL)
	}
	want := map[string]any{
		"estimated_bloat_pct": 40.0, "bloat_bytes": int64(8000 * 8192),
		"index_bytes": int64(20000 * 8192), "relpages": int64(20000),
		"estimated_pages": int64(12000), "fillfactor": 90, "threshold_pct": 30,
		"table": "App.orders", "estimate_source": "catalog_statistics",
	}
	for k, v := range want {
		if f.Detail[k] != v {
			t.Errorf("Detail[%s] = %#v, want %#v", k, f.Detail[k], v)
		}
	}
	if f.Title == "" || f.Recommendation == "" {
		t.Fatalf("title %q / recommendation %q must be set", f.Title, f.Recommendation)
	}
}

// Nil/empty: no rows (nil or empty) give no findings;
// index_bloat_threshold_pct = 0 falls back to the default 30.
func TestIndexBloat_EmptyAndDefaultThreshold(t *testing.T) {
	if got := ruleIndexBloat(&collector.Snapshot{}, nil, bloatConfig(30), nil); len(got) != 0 {
		t.Fatalf("nil rows: findings %+v", got)
	}
	if got := ruleIndexBloat(bloatSnapshot(), nil, bloatConfig(30), nil); len(got) != 0 {
		t.Fatalf("empty rows: findings %+v", got)
	}
	snap := bloatSnapshot(bloatRow("public", "a_idx", 29), bloatRow("public", "b_idx", 30))
	got := ruleIndexBloat(snap, nil, bloatConfig(0), nil)
	if len(got) != 1 || got[0].ObjectIdentifier != "public.b_idx" ||
		got[0].Detail["threshold_pct"] != 30 {
		t.Fatalf("threshold 0: findings %+v, want only public.b_idx at the default 30", got)
	}
}

// Invalid input: a row without pages, or with a negative or NaN estimate,
// is never reported, even at a threshold of 1%.
func TestIndexBloat_ImplausibleRowsSkipped(t *testing.T) {
	noPages := bloatRow("public", "nopages_idx", 80)
	noPages.RelPages = 0
	negative := bloatRow("public", "neg_idx", -10)
	nan := bloatRow("public", "nan_idx", math.NaN())
	got := ruleIndexBloat(bloatSnapshot(noPages, negative, nan), nil, bloatConfig(1), nil)
	if len(got) != 0 {
		t.Fatalf("findings %+v, want none for implausible rows", got)
	}
}

// State: the rule is registered for index_bloat; its input is known only
// when the category was read in full this cycle, so an unread, failed or
// truncated reading never resolves an open finding, while an empty one
// does (the index was rebuilt or shrank).
func TestIndexBloat_RuleNeeds(t *testing.T) {
	needs := ruleNeeds(t, "index_bloat")
	var spec RuleSpec
	for _, r := range AllRules {
		if r.Name == "index_bloat" {
			spec = r
		}
	}
	if len(spec.Categories) != 1 || spec.Categories[0] != "index_bloat" {
		t.Fatalf("index_bloat rule categories = %v", spec.Categories)
	}
	failed := bloatSnapshot()
	failed.Unavailable = map[string]string{"index_bloat": "statement timeout"}
	truncated := bloatSnapshot(bloatRow("public", "a_idx", 10))
	truncated.IndexBloatTruncated = true
	cases := []struct {
		name string
		snap *collector.Snapshot
		want bool
	}{
		{"not read (nil)", &collector.Snapshot{}, false},
		{"read, empty", bloatSnapshot(), true},
		{"read, rows", bloatSnapshot(bloatRow("public", "a_idx", 10)), true},
		{"unavailable", failed, false},
		{"truncated at the row cap", truncated, false},
	}
	for _, tc := range cases {
		if got := needs(tc.snap, nil); got != tc.want {
			t.Errorf("%s: needs = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// State transition: after a rebuild the next cycle reads the index below
// the threshold; the category is evaluated, so the open finding resolves.
func TestIndexBloat_RebuiltIndexResolvesNextCycle(t *testing.T) {
	cfg := phase2Config()
	cfg.Analyzer.IndexBloatThresholdPct = 30
	a := New(nil, cfg, nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	first := a.runSnapshotRules(bloatSnapshot(bloatRow("public", "o_idx", 70)), nil, true)
	if f := findingFor(first, "public.o_idx"); f == nil || f.Category != "index_bloat" {
		t.Fatalf("cycle 1: findings %+v, want index_bloat on public.o_idx", first)
	}
	a.eval = newCycleEval()
	second := a.runSnapshotRules(bloatSnapshot(bloatRow("public", "o_idx", 2)), nil, true)
	if f := findingFor(second, "public.o_idx"); f != nil && f.Category == "index_bloat" {
		t.Fatalf("cycle 2: finding %+v after the rebuild", f)
	}
	if !a.eval.resolvable(nil)["index_bloat"] {
		t.Fatal("cycle 2: index_bloat not evaluated, its open finding would never resolve")
	}
	a.eval = newCycleEval()
	failed := bloatSnapshot()
	failed.Unavailable = map[string]string{"index_bloat": "statement timeout"}
	a.runSnapshotRules(failed, nil, true)
	if a.eval.resolvable(nil)["index_bloat"] {
		t.Fatal("failed read: index_bloat claimed evaluated")
	}
}

// Review (rules-13), state: an index read this cycle without an estimate
// (a key column lost its statistics) is no verdict. The category still
// resolves, but that index's open finding is kept, while a reading
// without unknown indexes keeps nothing.
func TestIndexBloat_UnknownEstimateHoldsOpenFinding(t *testing.T) {
	cfg := phase2Config()
	a := New(nil, cfg, nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	snap := bloatSnapshot(bloatRow("public", "clean_idx", 2))
	snap.IndexBloatUnknown = []string{"public.o_idx"}
	a.runSnapshotRules(snap, nil, true)
	if !a.eval.resolvable(nil)["index_bloat"] {
		t.Fatal("index_bloat not evaluated with a complete, untruncated reading")
	}
	kept := a.eval.kept("index_bloat")
	if !kept["public.o_idx"] || kept["public.clean_idx"] || len(kept) != 1 {
		t.Fatalf("kept = %v, want only public.o_idx held open", kept)
	}
	a.eval = newCycleEval()
	a.runSnapshotRules(bloatSnapshot(bloatRow("public", "clean_idx", 2)), nil, true)
	if kept := a.eval.kept("index_bloat"); len(kept) != 0 {
		t.Fatalf("no unknown indexes: kept = %v, want none", kept)
	}
}
