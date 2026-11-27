package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// CatalogSQLFindings fires every analyzer source that writes
// RecommendedSQL on a fixture, keyed by its source file. The executor's
// target-parity test (catalog_target_parity_test.go, executor-08) checks
// that each statement changes the object its finding names.
func CatalogSQLFindings(t *testing.T) map[string][]Finding {
	t.Helper()
	out := indexCatalogFindings()
	for file, findings := range tableCatalogFindings() {
		out[file] = findings
	}
	for file, findings := range sessionCatalogFindings() {
		out[file] = findings
	}
	return out
}

func indexCatalogFindings() map[string][]Finding {
	invalidExtras := &RuleExtras{InvalidFirstSeen: map[string]time.Time{},
		IndexBuildTables: map[string]bool{}}
	ruleInvalidIndexes(invalidSnap(), nil, nil, invalidExtras)
	invalid := ruleInvalidIndexes(invalidSnap(), nil, nil, invalidExtras)
	old := time.Now().Add(-30 * 24 * time.Hour)
	unusedExtras := &RuleExtras{FirstSeen: map[string]time.Time{"public.idx_x": old},
		RecentlyCreated: map[string]time.Time{}, StatsEpoch: old}
	exact := duplicateSnap("CREATE INDEX idx_b ON public.orders USING btree (id)", "idx_b")
	subset := duplicateSnap("CREATE INDEX idx_ab ON public.orders USING btree (id, name)",
		"idx_ab")
	duplicates := append(ruleDuplicateIndexes(exact, nil, nil, nil),
		ruleDuplicateIndexes(subset, nil, nil, nil)...)
	return map[string][]Finding{
		"rules_index.go": append(invalid, duplicates...),
		"rules_unused_index.go": ruleUnusedIndexes(unusedIndexSnap(0), nil, unusedCfg(),
			unusedExtras),
		"rules_fk_index.go": ruleMissingFKIndexes(fkSnap(nil,
			[]collector.ForeignKey{fk("customer_id", "orders_customer_fk")}), nil, nil, nil),
		"optimizer_mapping.go": {OptimizerRecommendationFinding(optimizer.Recommendation{
			Table: "public.orders", Severity: "warning",
			DDL: "CREATE INDEX CONCURRENTLY idx_o ON public.orders (customer_id)"})},
	}
}

func duplicateSnap(def, name string) *collector.Snapshot {
	return &collector.Snapshot{Indexes: []collector.IndexStats{
		{SchemaName: "public", RelName: "orders", IndexRelName: "idx_a", IsValid: true,
			IdxScan: 100, IndexDef: "CREATE INDEX idx_a ON public.orders USING btree (id)"},
		{SchemaName: "public", RelName: "orders", IndexRelName: name, IsValid: true,
			IdxScan: 10, IndexDef: def},
	}}
}

func tableCatalogFindings() map[string][]Finding {
	cfg := testConfig()
	cfg.Analyzer.AnalyzeStaleMinRows = 10000
	cfg.Analyzer.AnalyzeStaleDays = 7
	now := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	stale := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "Sales", RelName: "Events", NLiveTup: 50000, NTupIns: 50000,
			ModSinceAnalyze: 50000, AnalyzeThreshold: 50}}}
	bloat := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "cache_data", NLiveTup: 5000, NDeadTup: 4000}}}
	indexBloat := &collector.Snapshot{IndexBloat: []collector.IndexBloat{
		{SchemaName: "Sales", RelName: "Events", IndexRelName: "Events_ts_idx",
			RelPages: 1000, EstimatedPages: 300, BloatPct: 70}}}
	return map[string][]Finding{
		"rules_autovacuum_tuning.go": ruleAutovacuumTuning(
			autovacuumSnap("autovacuum_vacuum_scale_factor=0.2"), nil, phase2Config(), nil),
		"rules_stale_statistics.go": ruleStaleStatisticsAt(stale, cfg, now),
		"rules_stale_statistics_parent.go": staleParentFindings(staleParentFixture(),
			newStaleWindow(cfg, now)),
		"rules_vacuum.go":      ruleTableBloat(bloat, nil, cfg, nil),
		"rules_index_bloat.go": ruleIndexBloat(indexBloat, nil, cfg, nil),
	}
}

func sessionCatalogFindings() map[string][]Finding {
	since := time.Now().Add(-2 * time.Hour)
	idle := LockChain{RootBlockerPID: 500, RootBlockerApp: "app",
		RootBlockerState: "idle in transaction",
		RootBlockerSince: since, TotalBlocked: 5, BlockedPIDs: make([]int, 5), ChainDepth: 2}
	active := idle
	active.RootBlockerState = "active"
	lc := baseLockChainConfig()
	slots := slotSnapshot([]collector.SlotInfo{{SlotName: "stale_slot", Active: false,
		SlotType: "logical", RetainedBytes: 50 << 30}})
	return map[string][]Finding{
		"rules_lockchain.go": {buildActionableFinding(idle, lc),
			buildActionableFinding(active, lc)},
		"rules_replication.go":       ruleInactiveSlots(slots, nil, nil, nil),
		"rules_system.go":            ruleConnectionLeaks([]LeakedConn{{PID: 42}}),
		"rules_workmem_promotion.go": {buildWorkMemPromotionFinding("app_worker", 5, 256, 5)},
		"rules_plan_flip.go": {buildPlanFlipFinding(Finding{Severity: "warning"},
			planPair{QueryID: 42}, flipCauseT{Schema: "public", Name: "orders", At: since},
			blocksPerCall{}, nil, time.Now())},
	}
}

// staleParentFixture is a mixed-case partitioned parent whose partitions
// changed past its threshold (rules-10).
func staleParentFixture() *collector.Snapshot {
	return &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "Sales", RelName: "Events", Relkind: "p", AnalyzeThreshold: 50},
			{SchemaName: "Sales", RelName: "Events_1", Relkind: "r", NLiveTup: 20000,
				ModSinceAnalyze: 900, AnalyzeThreshold: 2050}},
		Partitions: []collector.PartitionInfo{{ChildSchema: "Sales", ChildTable: "Events_1",
			ParentSchema: "Sales", ParentTable: "Events"}},
	}
}
