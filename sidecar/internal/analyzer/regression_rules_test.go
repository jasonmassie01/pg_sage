package analyzer

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// Regression tests for rule bugs recorded in docs/reviews/2026-09-26.

// G2-B04/C01/G1-B08: the collector historically emitted a percent while
// the rule compared a fraction. Both encodings must be interpreted as the
// same ratio.
func TestRegression_CacheHitRatioUnits(t *testing.T) {
	cfg := phase2Config()
	cases := []struct {
		ratio   float64
		wantSev string
		wantPct string
	}{
		{60.0, "critical", "60.00%"},
		{0.60, "critical", "60.00%"},
		{90.0, "warning", "90.00%"},
		{0.90, "warning", "90.00%"},
		{99.5, "", ""},
		{0.995, "", ""},
		{-1, "", ""},
		{0, "", ""},
	}
	for _, tc := range cases {
		snap := &collector.Snapshot{}
		snap.System.CacheHitRatio = tc.ratio
		got := ruleCacheHitRatio(snap, nil, cfg, nil)
		if tc.wantSev == "" {
			if len(got) != 0 {
				t.Errorf("ratio %v: got %d findings, want 0", tc.ratio, len(got))
			}
			continue
		}
		if len(got) != 1 {
			t.Fatalf("ratio %v: got %d findings, want 1", tc.ratio, len(got))
		}
		if got[0].Severity != tc.wantSev {
			t.Errorf("ratio %v: severity %q, want %q",
				tc.ratio, got[0].Severity, tc.wantSev)
		}
		if !strings.Contains(got[0].Title, tc.wantPct) {
			t.Errorf("ratio %v: title %q lacks %q", tc.ratio, got[0].Title, tc.wantPct)
		}
	}
}

func perTableAutovacuum(ident string) Finding {
	return Finding{
		Category:         "autovacuum_tuning",
		Severity:         "warning",
		ObjectIdentifier: ident,
		Title:            "tune " + ident,
		RecommendedSQL: "ALTER TABLE " + ident +
			" SET (autovacuum_vacuum_scale_factor = 0.05);",
	}
}

// G2-B05: per-table GUC recommendations on different tables do not conflict.
func TestRegression_PerTableGUCsDoNotConflictAcrossTables(t *testing.T) {
	in := []Finding{
		perTableAutovacuum("public.a"),
		perTableAutovacuum("public.b"),
		perTableAutovacuum("public.c"),
	}
	got := DeduplicateFindings(in, 0, noopLog)
	if len(got) != 3 {
		t.Fatalf("findings after dedup = %d, want 3", len(got))
	}
}

func autovacuumSnap(reloptions string) *collector.Snapshot {
	snap := &collector.Snapshot{Tables: []collector.TableStats{{
		SchemaName: "public", RelName: "events",
		NLiveTup: 2_000_000, NTupUpd: 3_000_000,
	}}}
	if reloptions != "" {
		snap.ConfigData = &collector.ConfigSnapshot{
			TableReloptions: []collector.TableReloption{{
				SchemaName: "public", RelName: "events",
				Reloptions: reloptions,
			}},
		}
	}
	return snap
}

// G2-B05: a table already tuned at or below the target is not re-flagged.
func TestRegression_AutovacuumTuningHonorsReloptions(t *testing.T) {
	cfg := phase2Config()
	tuned := autovacuumSnap("{autovacuum_vacuum_scale_factor=0.05,fillfactor=90}")
	if got := ruleAutovacuumTuning(tuned, nil, cfg, nil); len(got) != 0 {
		t.Fatalf("already-tuned table flagged: %+v", got)
	}
	loose := autovacuumSnap("{autovacuum_vacuum_scale_factor=0.2}")
	if got := ruleAutovacuumTuning(loose, nil, cfg, nil); len(got) != 1 {
		t.Fatalf("loosely tuned table findings = %d, want 1", len(got))
	}
}

func unusedIndexSnap(scans int64) *collector.Snapshot {
	return &collector.Snapshot{Indexes: []collector.IndexStats{{
		SchemaName: "public", RelName: "orders", IndexRelName: "idx_x",
		IsValid: true, IdxScan: scans,
		IndexDef: "CREATE INDEX idx_x ON public.orders USING btree (x)",
	}}}
}

func unusedCfg() *config.Config {
	cfg := &config.Config{}
	cfg.Analyzer.UnusedIndexWindowDays = 7
	return cfg
}

// G2-B07: FirstSeen must reset once the index is used, so a later stats
// reset does not immediately trigger a DROP recommendation.
func TestRegression_UnusedIndexFirstSeenClearedOnUse(t *testing.T) {
	extras := &RuleExtras{
		FirstSeen:       map[string]time.Time{"public.idx_x": time.Now().Add(-30 * 24 * time.Hour)},
		RecentlyCreated: map[string]time.Time{},
	}
	ruleUnusedIndexes(unusedIndexSnap(42), nil, unusedCfg(), extras)
	got := ruleUnusedIndexes(unusedIndexSnap(0), nil, unusedCfg(), extras)
	if len(got) != 0 {
		t.Fatalf("finding right after counter reset: %+v", got)
	}
}

// G2-B07: an index cannot be called unused for longer than the stats have
// existed (pg_stat_reset / restart).
func TestRegression_UnusedIndexRespectsStatsEpoch(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	extras := &RuleExtras{
		FirstSeen:       map[string]time.Time{"public.idx_x": old},
		RecentlyCreated: map[string]time.Time{},
		StatsEpoch:      time.Now().Add(-time.Hour),
	}
	if got := ruleUnusedIndexes(unusedIndexSnap(0), nil, unusedCfg(), extras); len(got) != 0 {
		t.Fatalf("finding within stats epoch window: %+v", got)
	}
	extras.StatsEpoch = old
	if got := ruleUnusedIndexes(unusedIndexSnap(0), nil, unusedCfg(), extras); len(got) != 1 {
		t.Fatalf("control: findings = %d, want 1", len(got))
	}
}

func fkSnap(indexDefs map[string]string, fks []collector.ForeignKey) *collector.Snapshot {
	snap := &collector.Snapshot{
		Tables:      []collector.TableStats{{SchemaName: "public", RelName: "orders"}},
		ForeignKeys: fks,
	}
	for name, def := range indexDefs {
		snap.Indexes = append(snap.Indexes, collector.IndexStats{
			SchemaName: "public", RelName: "orders", IndexRelName: name,
			IsValid: true, IdxScan: 1, IndexDef: def,
		})
	}
	return snap
}

func fk(col, con string) collector.ForeignKey {
	return collector.ForeignKey{
		TableName: "orders", ReferencedTable: "parent",
		FKColumn: col, ConstraintName: con,
	}
}

// G2-B08: INCLUDE columns and composite FKs must be recognized.
func TestRegression_MissingFKIndexIncludeAndComposite(t *testing.T) {
	include := fkSnap(map[string]string{
		"idx_c": "CREATE INDEX idx_c ON public.orders USING btree (customer_id) INCLUDE (total)",
	}, []collector.ForeignKey{fk("customer_id", "orders_customer_fk")})
	if got := ruleMissingFKIndexes(include, nil, nil, nil); len(got) != 0 {
		t.Fatalf("INCLUDE index not recognized: %+v", got)
	}
	composite := fkSnap(map[string]string{
		"idx_ot": "CREATE INDEX idx_ot ON public.orders USING btree (order_id, tenant_id)",
	}, []collector.ForeignKey{
		fk("tenant_id", "orders_line_fk"), fk("order_id", "orders_line_fk"),
	})
	if got := ruleMissingFKIndexes(composite, nil, nil, nil); len(got) != 0 {
		t.Fatalf("composite FK split per column: %+v", got)
	}
}

func tenantSnap() *collector.Snapshot {
	return &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "tenant_a", RelName: "orders"},
			{SchemaName: "tenant_b", RelName: "orders"},
		},
		ForeignKeys: []collector.ForeignKey{fk("customer_id", "orders_customer_fk")},
		Indexes: []collector.IndexStats{{
			SchemaName: "tenant_b", RelName: "orders", IndexRelName: "idx_cust",
			IsValid: true, IdxScan: 0,
			IndexDef: "CREATE INDEX idx_cust ON tenant_b.orders USING btree (customer_id)",
		}},
	}
}

// G2-B08: an FK whose table name exists in several schemas must not be
// attributed to a guessed schema (wrong CREATE INDEX, missing protection).
func TestRegression_FKSchemaNotGuessed(t *testing.T) {
	for _, f := range ruleMissingFKIndexes(tenantSnap(), nil, nil, nil) {
		if strings.HasPrefix(f.ObjectIdentifier, "tenant_a.") {
			t.Fatalf("finding attributed to guessed schema: %s", f.ObjectIdentifier)
		}
	}
	extras := &RuleExtras{
		FirstSeen: map[string]time.Time{
			"tenant_b.idx_cust": time.Now().Add(-30 * 24 * time.Hour),
		},
		RecentlyCreated: map[string]time.Time{},
	}
	if got := ruleUnusedIndexes(tenantSnap(), nil, unusedCfg(), extras); len(got) != 0 {
		t.Fatalf("possible sole FK index recommended for drop: %+v", got)
	}
}

func invalidSnap() *collector.Snapshot {
	return &collector.Snapshot{Indexes: []collector.IndexStats{{
		SchemaName: "public", RelName: "orders", IndexRelName: "idx_building",
		IsValid:  false,
		IndexDef: "CREATE INDEX idx_building ON public.orders USING btree (x)",
	}}}
}

// G2-B09: an index being built concurrently is invalid for the whole
// build and must not be recommended for DROP.
func TestRegression_InvalidIndexBuildInProgress(t *testing.T) {
	extras := &RuleExtras{
		InvalidFirstSeen: map[string]time.Time{},
		IndexBuildTables: map[string]bool{"public.orders": true},
	}
	for i := 0; i < 3; i++ {
		if got := ruleInvalidIndexes(invalidSnap(), nil, nil, extras); len(got) != 0 {
			t.Fatalf("cycle %d: in-progress build flagged: %+v", i, got)
		}
	}
}

// G2-B09: an invalid index must be observed in two cycles before a drop
// is recommended, and never when the build probe failed.
func TestRegression_InvalidIndexNeedsTwoObservations(t *testing.T) {
	extras := &RuleExtras{
		InvalidFirstSeen: map[string]time.Time{},
		IndexBuildTables: map[string]bool{},
	}
	if got := ruleInvalidIndexes(invalidSnap(), nil, nil, extras); len(got) != 0 {
		t.Fatalf("first observation flagged: %+v", got)
	}
	if got := ruleInvalidIndexes(invalidSnap(), nil, nil, extras); len(got) != 1 {
		t.Fatalf("second observation findings = %d, want 1", len(got))
	}
	extras.IndexBuildProbeFailed = true
	if got := ruleInvalidIndexes(invalidSnap(), nil, nil, extras); len(got) != 0 {
		t.Fatalf("flagged while build probe failed: %+v", got)
	}
}
