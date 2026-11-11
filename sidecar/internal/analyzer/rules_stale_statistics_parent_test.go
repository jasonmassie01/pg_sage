package analyzer

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rules-10: autoanalyze never processes a partitioned parent, so its
// statistics go stale while its partitions change. The rule raises an
// info finding for the parent from the partitions' modifications.

// No concurrent access tests: the parent check is a pure function of the
// snapshot, config and clock.

func parentTree(parentLast *time.Time, leafMods ...int64) *collector.Snapshot {
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "events", Relkind: "p",
			AnalyzeThreshold: 50, LastAnalyze: parentLast}}}
	for i, mods := range leafMods {
		name := "events_" + string(rune('a'+i))
		snap.Tables = append(snap.Tables, collector.TableStats{SchemaName: "public",
			RelName: name, Relkind: "r", NLiveTup: 6000,
			ModSinceAnalyze: mods, AnalyzeThreshold: 650})
		snap.Partitions = append(snap.Partitions, collector.PartitionInfo{
			ChildSchema: "public", ChildTable: name,
			ParentSchema: "public", ParentTable: "events"})
	}
	return snap
}

func TestRuleStaleStatistics_PartitionedParentNeverAnalyzed(t *testing.T) {
	snap := parentTree(nil, 30, 40)
	got := staleIdents(ruleStaleStatisticsAt(snap, rules10Config(), rules10Now))
	f, ok := got["public.events"]
	if !ok || len(got) != 1 {
		t.Fatalf("findings = %v, want the parent public.events only", got)
	}
	if f.Severity != "info" || f.ObjectType != "table" || f.ActionRisk != "safe" {
		t.Errorf("severity/type/risk = %s/%s/%s, want info/table/safe",
			f.Severity, f.ObjectType, f.ActionRisk)
	}
	if f.Detail["partitioned_parent"] != true || f.Detail["mod_since_analyze"] != int64(70) ||
		f.Detail["analyze_threshold"] != float64(50) || f.Detail["never_analyzed"] != true ||
		f.Detail["partitions"] != 2 {
		t.Errorf("detail = %v", f.Detail)
	}
	if f.RecommendedSQL != `ANALYZE "public"."events";` {
		t.Errorf("RecommendedSQL = %q", f.RecommendedSQL)
	}
	reason, _ := f.Detail[DetailApprovalRequired].(string)
	if !strings.Contains(reason, "every partition") {
		t.Errorf("approval_required = %q, want the full-tree sampling caveat", reason)
	}
	if !strings.Contains(f.Recommendation, "ANALYZE ONLY") {
		t.Errorf("recommendation %q does not name ANALYZE ONLY for PostgreSQL 18",
			f.Recommendation)
	}
}

func TestRuleStaleStatistics_PartitionedParentBoundary(t *testing.T) {
	at := staleIdents(ruleStaleStatisticsAt(parentTree(nil, 20, 30),
		rules10Config(), rules10Now))
	if len(at) != 0 {
		t.Errorf("partition modifications == parent threshold flagged: %v", at)
	}
	past := staleIdents(ruleStaleStatisticsAt(parentTree(nil, 20, 31),
		rules10Config(), rules10Now))
	if _, ok := past["public.events"]; !ok {
		t.Errorf("threshold + 1 gave no parent finding: %v", past)
	}
}

func TestRuleStaleStatistics_PartitionedParentCutoff(t *testing.T) {
	recent, old := rules10Now.AddDate(0, 0, -1), rules10Now.AddDate(0, 0, -30)
	if got := ruleStaleStatisticsAt(parentTree(&recent, 300, 400),
		rules10Config(), rules10Now); len(got) != 0 {
		t.Errorf("recently analyzed parent flagged: %+v", got)
	}
	got := staleIdents(ruleStaleStatisticsAt(parentTree(&old, 300, 400),
		rules10Config(), rules10Now))
	f, ok := got["public.events"]
	if !ok || f.Detail["never_analyzed"] != false {
		t.Errorf("stale parent findings = %v, want public.events analyzed long ago", got)
	}
}

// State transition: the parent is analyzed (its partitions with it), the
// partitions' modifications reset, and the finding is gone.
func TestRuleStaleStatistics_PartitionedParentClearsAfterAnalyze(t *testing.T) {
	if got := ruleStaleStatisticsAt(parentTree(nil, 300, 400),
		rules10Config(), rules10Now); len(got) != 1 {
		t.Fatalf("before ANALYZE: %d findings, want 1", len(got))
	}
	analyzed := rules10Now
	for week := 1; week <= 4; week++ {
		at := rules10Now.AddDate(0, 0, 7*week)
		if got := ruleStaleStatisticsAt(parentTree(&analyzed, 0, 0),
			rules10Config(), at); len(got) != 0 {
			t.Fatalf("week %d after ANALYZE: %+v", week, got)
		}
	}
}

// Nil/empty: without the partitions extra the parent is not judged (its
// partitions' changes are unknown), and a parent with no partitions has
// nothing to sum.
func TestRuleStaleStatistics_PartitionedParentNeedsPartitions(t *testing.T) {
	snap := parentTree(nil, 300, 400)
	snap.Unavailable = map[string]string{"partitions": "statement timeout"}
	if got := ruleStaleStatisticsAt(snap, rules10Config(), rules10Now); len(got) != 0 {
		t.Errorf("partitions unavailable, yet findings %+v", got)
	}
	bare := parentTree(nil)
	if got := ruleStaleStatisticsAt(bare, rules10Config(), rules10Now); len(got) != 0 {
		t.Errorf("parent without partitions flagged: %+v", got)
	}
}

// Invalid input: a listed partition missing from the tables (dropped
// between the two reads) makes the sum unknown, so the parent is skipped.
func TestRuleStaleStatistics_PartitionedParentMissingLeafSkipped(t *testing.T) {
	snap := parentTree(nil, 300, 400)
	snap.Tables = snap.Tables[:2] // events_b listed but not collected
	// events_a alone clears the size floor and the threshold, so only the
	// missing leaf keeps the parent from being judged on a partial sum. The
	// analyzed leaf is not flagged itself.
	analyzed := rules10Now.AddDate(0, 0, -1)
	snap.Tables[1].NLiveTup, snap.Tables[1].LastAnalyze = 20000, &analyzed
	if got := ruleStaleStatisticsAt(snap, rules10Config(), rules10Now); len(got) != 0 {
		t.Errorf("parent judged on a partial sum: %+v", got)
	}
}

// Sub-partitions: the top parent sums its leaves through an intermediate
// partitioned table, and each partitioned level gets its own finding.
func TestRuleStaleStatistics_PartitionedParentSumsSubPartitions(t *testing.T) {
	snap := parentTree(nil, 10)
	leafAnalyzed := rules10Now.AddDate(0, 0, -1) // a large leaf, analyzed: not flagged itself
	snap.Tables = append(snap.Tables,
		collector.TableStats{SchemaName: "public", RelName: "events_mid", Relkind: "p",
			AnalyzeThreshold: 50},
		collector.TableStats{SchemaName: "public", RelName: "events_mid_1", Relkind: "r",
			NLiveTup: 12000, ModSinceAnalyze: 60, AnalyzeThreshold: 1250,
			LastAnalyze: &leafAnalyzed})
	snap.Partitions = append(snap.Partitions,
		collector.PartitionInfo{ChildSchema: "public", ChildTable: "events_mid",
			ParentSchema: "public", ParentTable: "events"},
		collector.PartitionInfo{ChildSchema: "public", ChildTable: "events_mid_1",
			ParentSchema: "public", ParentTable: "events_mid"})
	got := staleIdents(ruleStaleStatisticsAt(snap, rules10Config(), rules10Now))
	if got["public.events"].Detail["mod_since_analyze"] != int64(70) {
		t.Errorf("top parent = %v, want mod_since_analyze 70 over both levels",
			got["public.events"].Detail)
	}
	if got["public.events_mid"].Detail["mod_since_analyze"] != int64(60) {
		t.Errorf("intermediate parent = %v, want mod_since_analyze 60",
			got["public.events_mid"].Detail)
	}
	if len(got) != 2 {
		t.Errorf("findings = %v, want the two partitioned levels", got)
	}
}

// Invalid input: a malformed partition list with a cycle terminates and
// judges nothing it cannot sum.
func TestRuleStaleStatistics_PartitionCycleTerminates(t *testing.T) {
	snap := parentTree(nil, 300)
	snap.Partitions = append(snap.Partitions, collector.PartitionInfo{
		ChildSchema: "public", ChildTable: "events",
		ParentSchema: "public", ParentTable: "events"})
	done := make(chan []Finding, 1)
	go func() { done <- ruleStaleStatisticsAt(snap, rules10Config(), rules10Now) }()
	select {
	case got := <-done:
		if _, ok := staleIdents(got)["public.events"]; ok {
			t.Errorf("parent judged through a partition cycle: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("partition cycle did not terminate")
	}
}

// A small tree (partitions' live rows below analyze_stale_min_rows) is
// left alone, like a small table.
func TestRuleStaleStatistics_SmallPartitionedTreeSkipped(t *testing.T) {
	snap := parentTree(nil, 300, 400)
	snap.Tables[1].NLiveTup, snap.Tables[2].NLiveTup = 4000, 5999
	if got := ruleStaleStatisticsAt(snap, rules10Config(), rules10Now); len(got) != 0 {
		t.Errorf("small tree flagged: %+v", got)
	}
}

// A legacy inheritance parent (relkind r with children in pg_inherits) is
// judged as an ordinary table by its own counters, never as a parent.
func TestRuleStaleStatistics_InheritanceParentIsOrdinaryTable(t *testing.T) {
	snap := parentTree(nil, 300, 400)
	snap.Tables[0].Relkind = "r"
	if got := ruleStaleStatisticsAt(snap, rules10Config(), rules10Now); len(got) != 0 {
		t.Errorf("inheritance parent judged by its children: %+v", got)
	}
}
