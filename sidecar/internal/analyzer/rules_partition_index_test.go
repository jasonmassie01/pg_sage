package analyzer

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// rules-03: the index rules know partitioned indexes. An index attached to
// a partitioned index cannot be dropped on its own and a partitioned table
// cannot take CREATE INDEX CONCURRENTLY, so neither statement is ever
// recommended; an unused partitioned index rolls up into one advisory
// finding on the parent.
//
// No concurrent access tests: the rules are pure functions over one
// snapshot, run on the analyzer's single cycle goroutine.

var partNow = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func partExtras() *RuleExtras {
	e := defaultExtras()
	e.Now = func() time.Time { return partNow }
	return e
}

// markUnused starts the zero-scan clock of each index d ago.
func markUnused(e *RuleExtras, d time.Duration, idents ...string) {
	for _, id := range idents {
		e.FirstSeen[id] = partNow.Add(-d)
	}
}

const eightDays = 8 * 24 * time.Hour

// partitionedSnap is public.events partitioned into events_p1..events_pN,
// with the partitioned index public.events_ts_idx (oid 1000) attached on
// every partition as events_pN_ts_idx (oid 1000+N).
func partitionedSnap(n int) *collector.Snapshot {
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{{SchemaName: "public", RelName: "events", Relkind: "p"}},
		PartitionedIndexes: []collector.PartitionedIndex{
			partIndex("events", "events_ts_idx", 1000, "ts"),
		},
	}
	for i := 1; i <= n; i++ {
		part := fmt.Sprintf("events_p%d", i)
		snap.Tables = append(snap.Tables,
			collector.TableStats{SchemaName: "public", RelName: part, Relkind: "r"})
		snap.Partitions = append(snap.Partitions, collector.PartitionInfo{
			ChildSchema: "public", ChildTable: part,
			ParentSchema: "public", ParentTable: "events",
		})
		snap.Indexes = append(snap.Indexes,
			childIndex(part, part+"_ts_idx", "ts", uint32(1000+i), 1000))
	}
	return snap
}

func partIndex(table, name string, oid uint32, cols ...string) collector.PartitionedIndex {
	return collector.PartitionedIndex{SchemaName: "public", RelName: table,
		IndexRelName: name, IndexRelID: oid, KeyColumns: cols, IsValid: true}
}

func childIndex(table, name, cols string, oid, parent uint32) collector.IndexStats {
	return collector.IndexStats{SchemaName: "public", RelName: table,
		IndexRelName: name, IsValid: true, IndexType: "btree",
		IndexRelID: oid, ParentIndexOID: parent,
		IndexDef: fmt.Sprintf("CREATE INDEX %s ON public.%s USING btree (%s)",
			name, table, cols)}
}

func childIdents(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("public.events_p%d_ts_idx", i+1)
	}
	return out
}

func identsOf(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.ObjectIdentifier)
	}
	return out
}

func TestPartitionIndex_AttachedChildNeverFlagged(t *testing.T) {
	snap := partitionedSnap(2)
	snap.Indexes[1].IdxScan = 5 // a used sibling: no roll-up either
	e := partExtras()
	markUnused(e, eightDays, "public.events_p1_ts_idx")
	got := ruleUnusedIndexes(snap, nil, unusedCfg(), e)
	if len(got) != 0 {
		t.Fatalf("findings = %v, want none for an attached child (PostgreSQL rejects "+
			"DROP INDEX on it)", identsOf(got))
	}
	if _, ok := e.FirstSeen["public.events_p1_ts_idx"]; !ok {
		t.Error("the attached child's unused clock was dropped; the roll-up needs it")
	}
}

func TestPartitionIndex_StandaloneOnPartitionStillFlagged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		detached bool
	}{{"never attached", false}, {"partition detached", true}} {
		t.Run(tc.name, func(t *testing.T) {
			snap := partitionedSnap(2)
			snap.Indexes[0].IdxScan, snap.Indexes[1].IdxScan = 5, 5
			snap.Indexes = append(snap.Indexes,
				childIndex("events_p1", "events_p1_extra", "k", 2001, 0))
			if tc.detached {
				snap.Partitions = snap.Partitions[1:]
			}
			e := partExtras()
			markUnused(e, eightDays, "public.events_p1_extra")
			got := ruleUnusedIndexes(snap, nil, unusedCfg(), e)
			if len(got) != 1 || got[0].ObjectIdentifier != "public.events_p1_extra" {
				t.Fatalf("findings = %v, want public.events_p1_extra only", identsOf(got))
			}
			want := `DROP INDEX CONCURRENTLY "public"."events_p1_extra";`
			if got[0].RecommendedSQL != want {
				t.Errorf("SQL = %q, want %q", got[0].RecommendedSQL, want)
			}
		})
	}
}

func TestPartitionIndex_RollupNeedsEveryChild(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*collector.Snapshot, *RuleExtras)
		want  int
	}{
		{"3 of 4 unused, 4th scanned", func(s *collector.Snapshot, e *RuleExtras) {
			markUnused(e, eightDays, childIdents(3)...)
			s.Indexes[3].IdxScan = 1
		}, 0},
		{"4th clock just short of the window", func(s *collector.Snapshot, e *RuleExtras) {
			markUnused(e, eightDays, childIdents(3)...)
			markUnused(e, 7*24*time.Hour-time.Minute, "public.events_p4_ts_idx")
		}, 0},
		{"4th clock exactly the window", func(s *collector.Snapshot, e *RuleExtras) {
			markUnused(e, eightDays, childIdents(3)...)
			markUnused(e, 7*24*time.Hour, "public.events_p4_ts_idx")
		}, 1},
		{"4 of 4 unused", func(s *collector.Snapshot, e *RuleExtras) {
			markUnused(e, eightDays, childIdents(4)...)
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap, e := partitionedSnap(4), partExtras()
			tc.setup(snap, e)
			got := ruleUnusedIndexes(snap, nil, unusedCfg(), e)
			if len(got) != tc.want {
				t.Fatalf("findings = %v, want %d", identsOf(got), tc.want)
			}
			if tc.want == 1 && got[0].ObjectIdentifier != "public.events_ts_idx" {
				t.Fatalf("roll-up on %q, want the parent public.events_ts_idx",
					got[0].ObjectIdentifier)
			}
		})
	}
}

func TestPartitionIndex_RollupFindingIsAdvisory(t *testing.T) {
	snap, e := partitionedSnap(4), partExtras()
	markUnused(e, 9*24*time.Hour, childIdents(3)...)
	markUnused(e, eightDays, "public.events_p4_ts_idx")
	got := ruleUnusedIndexes(snap, nil, unusedCfg(), e)
	if len(got) != 1 {
		t.Fatalf("findings = %v, want one roll-up", identsOf(got))
	}
	f := got[0]
	if f.Category != "unused_index" || f.ObjectType != "index" ||
		f.ObjectIdentifier != "public.events_ts_idx" {
		t.Fatalf("roll-up = %s/%s/%s", f.Category, f.ObjectType, f.ObjectIdentifier)
	}
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Errorf("roll-up carries SQL %q / %q, want none (advisory)",
			f.RecommendedSQL, f.RollbackSQL)
	}
	if got := f.Detail["manual_sql"]; got != `DROP INDEX "public"."events_ts_idx";` {
		t.Errorf("manual_sql = %v, want a plain DROP INDEX (no CONCURRENTLY)", got)
	}
	if reason, _ := f.Detail[DetailApprovalRequired].(string); reason == "" {
		t.Error("roll-up must require approval")
	}
	if f.ActionRisk != "high_risk" {
		t.Errorf("ActionRisk = %q, want high_risk", f.ActionRisk)
	}
	if !strings.Contains(f.Recommendation, "unused on 4 of 4 partitions") {
		t.Errorf("recommendation %q lacks \"unused on 4 of 4 partitions\"", f.Recommendation)
	}
	if f.Detail["partitions_unused"] != 4 || f.Detail["partitions_total"] != 4 {
		t.Errorf("detail counts = %v of %v, want 4 of 4",
			f.Detail["partitions_unused"], f.Detail["partitions_total"])
	}
	wantSince := partNow.Add(-eightDays).Format(time.RFC3339)
	if f.Detail["unused_since"] != wantSince {
		t.Errorf("unused_since = %v, want the latest child clock %s",
			f.Detail["unused_since"], wantSince)
	}
	if f.Detail["table"] != "events" || f.Detail["partitioned_index"] != true {
		t.Errorf("detail = %v, want table events and partitioned_index true", f.Detail)
	}
}

func TestPartitionIndex_AttachResolvesStandaloneFinding(t *testing.T) {
	snap, e := partitionedSnap(2), partExtras()
	snap.Indexes[0].IdxScan, snap.Indexes[1].IdxScan = 5, 5
	snap.Indexes = append(snap.Indexes,
		childIndex("events_p1", "events_p1_extra", "k", 2001, 0))
	markUnused(e, eightDays, "public.events_p1_extra")
	first := ruleUnusedIndexes(snap, nil, unusedCfg(), e)
	if len(first) != 1 || first[0].ObjectIdentifier != "public.events_p1_extra" {
		t.Fatalf("cycle 1 findings = %v, want the standalone index", identsOf(first))
	}
	// ALTER INDEX events_extra ATTACH PARTITION events_p1_extra; the new
	// parent's own child on p2 is fresh.
	next := partitionedSnap(2)
	next.Indexes[0].IdxScan, next.Indexes[1].IdxScan = 5, 5
	next.PartitionedIndexes = append(next.PartitionedIndexes,
		partIndex("events", "events_extra", 3000, "k"))
	next.Indexes = append(next.Indexes,
		childIndex("events_p1", "events_p1_extra", "k", 2001, 3000),
		childIndex("events_p2", "events_p2_extra", "k", 3002, 3000))
	if !hasIndexes(next, snap) {
		t.Fatal("the rule must run on the next cycle so the finding resolves")
	}
	if got := ruleUnusedIndexes(next, snap, unusedCfg(), e); len(got) != 0 {
		t.Fatalf("cycle 2 findings = %v, want none once the index is attached",
			identsOf(got))
	}
}

func TestPartitionIndex_NoPartitionsNoRollup(t *testing.T) {
	t.Run("parent without partitions", func(t *testing.T) {
		snap := partitionedSnap(0)
		snap.Indexes = []collector.IndexStats{childIndex("other", "other_k", "k", 9, 0)}
		snap.Indexes[0].IdxScan = 3
		if got := ruleUnusedIndexes(snap, nil, unusedCfg(), partExtras()); len(got) != 0 {
			t.Fatalf("findings = %v, want none", identsOf(got))
		}
	})
	t.Run("partitioned indexes unknown", func(t *testing.T) {
		snap, e := partitionedSnap(3), partExtras()
		snap.PartitionedIndexes = nil
		markUnused(e, eightDays, childIdents(3)...)
		if got := ruleUnusedIndexes(snap, nil, unusedCfg(), e); len(got) != 0 {
			t.Fatalf("findings = %v, want none without the parent's row", identsOf(got))
		}
	})
}

func TestPartitionIndex_NeedsPartitionCategories(t *testing.T) {
	base := func() *collector.Snapshot {
		s := partitionedSnap(2)
		s.ForeignKeys = []collector.ForeignKey{{Schema: "public", TableName: "events",
			ReferencedTable: "accounts", Columns: []string{"account_id"}, ConstraintName: "fk"}}
		return s
	}
	if s := base(); !hasIndexes(s, nil) || !hasForeignKeys(s, nil) {
		t.Fatal("complete snapshot must satisfy both needs")
	}
	for _, cat := range []string{"partitions", "partitioned_indexes"} {
		s := base()
		s.Unavailable = map[string]string{cat: "timeout"}
		if hasIndexes(s, nil) {
			t.Errorf("%s unavailable: unused_index would run (and resolve the roll-up)", cat)
		}
		if hasForeignKeys(s, nil) {
			t.Errorf("%s unavailable: missing_fk_index would run on partial input", cat)
		}
	}
}

func TestPartitionIndex_RollupSkipsProtectedParents(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*collector.PartitionedIndex)
	}{
		{"unique", func(p *collector.PartitionedIndex) { p.IsUnique = true }},
		{"primary", func(p *collector.PartitionedIndex) { p.IsPrimary, p.IsUnique = true, true }},
		{"invalid", func(p *collector.PartitionedIndex) { p.IsValid = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap, e := partitionedSnap(2), partExtras()
			tc.mod(&snap.PartitionedIndexes[0])
			markUnused(e, eightDays, childIdents(2)...)
			if got := ruleUnusedIndexes(snap, nil, unusedCfg(), e); len(got) != 0 {
				t.Fatalf("findings = %v, want none", identsOf(got))
			}
		})
	}
}

func TestPartitionIndex_RollupKeepsFKSupport(t *testing.T) {
	snap, e := partitionedSnap(2), partExtras()
	snap.PartitionedIndexes[0].KeyColumns = []string{"account_id"}
	for i := range snap.Indexes {
		snap.Indexes[i].IndexDef = strings.Replace(snap.Indexes[i].IndexDef,
			"(ts)", "(account_id)", 1)
	}
	for _, table := range []string{"events", "events_p1", "events_p2"} {
		snap.ForeignKeys = append(snap.ForeignKeys, collector.ForeignKey{Schema: "public",
			TableName: table, ReferencedTable: "accounts", Columns: []string{"account_id"},
			ConstraintName: "events_account_id_fkey"})
	}
	markUnused(e, eightDays, childIdents(2)...)
	if got := ruleUnusedIndexes(snap, nil, unusedCfg(), e); len(got) != 0 {
		t.Fatalf("findings = %v, want none: the index is the FK's only support",
			identsOf(got))
	}
}

func TestPartitionIndex_NestedRollsUpToTop(t *testing.T) {
	snap, e := partitionedSnap(2), partExtras()
	// events_p1 is itself partitioned: its index is a partitioned index
	// attached to the top one, with two leaves under it.
	snap.Indexes = snap.Indexes[1:]
	mid := partIndex("events_p1", "events_p1_ts_idx", 1100, "ts")
	mid.ParentIndexOID = 1000
	snap.PartitionedIndexes = append(snap.PartitionedIndexes, mid)
	snap.Indexes = append(snap.Indexes,
		childIndex("events_p1_a", "events_p1_a_ts_idx", "ts", 1101, 1100),
		childIndex("events_p1_b", "events_p1_b_ts_idx", "ts", 1102, 1100))
	markUnused(e, eightDays, "public.events_p2_ts_idx",
		"public.events_p1_a_ts_idx", "public.events_p1_b_ts_idx")
	got := ruleUnusedIndexes(snap, nil, unusedCfg(), e)
	if len(got) != 1 || got[0].ObjectIdentifier != "public.events_ts_idx" {
		t.Fatalf("findings = %v, want the top-level roll-up only", identsOf(got))
	}
	if got[0].Detail["partitions_total"] != 3 {
		t.Errorf("partitions_total = %v, want 3 leaves", got[0].Detail["partitions_total"])
	}
}

// CHECK-33: 365 daily partitions with 3 partitioned indexes each, all old
// and unscanned: about 1,095 child findings before, at most 3 now.
func TestPartitionIndex_365PartitionsRollUp(t *testing.T) {
	snap, e := &collector.Snapshot{}, partExtras()
	for k, col := range []string{"ts", "user_id", "kind"} {
		parent := uint32(10_000 * (k + 1))
		snap.PartitionedIndexes = append(snap.PartitionedIndexes,
			partIndex("events", "events_"+col+"_idx", parent, col))
		for d := 1; d <= 365; d++ {
			part := fmt.Sprintf("events_d%03d", d)
			name := part + "_" + col + "_idx"
			snap.Indexes = append(snap.Indexes,
				childIndex(part, name, col, parent+uint32(d), parent))
			markUnused(e, 30*24*time.Hour, "public."+name)
		}
	}
	got := ruleUnusedIndexes(snap, nil, unusedCfg(), e)
	if len(got) > 3 || len(got) == 0 {
		t.Fatalf("findings = %d, want 1..3 roll-ups (was ~1,095)", len(got))
	}
	for _, f := range got {
		if f.RecommendedSQL != "" {
			t.Errorf("%s carries SQL %q", f.ObjectIdentifier, f.RecommendedSQL)
		}
	}
}

func TestPartitionIndex_DuplicateAttachedChildrenSilent(t *testing.T) {
	snap := partitionedSnap(1)
	snap.PartitionedIndexes = append(snap.PartitionedIndexes,
		partIndex("events", "events_ts_dup", 1500, "ts"))
	snap.Indexes = append(snap.Indexes,
		childIndex("events_p1", "events_p1_ts_dup", "ts", 1501, 1500))
	if got := ruleDuplicateIndexes(snap, nil, unusedCfg(), nil); len(got) != 0 {
		t.Fatalf("findings = %v, want none over attached children", identsOf(got))
	}
}

func TestPartitionIndex_DuplicateDropsTheStandaloneOne(t *testing.T) {
	snap := partitionedSnap(1)
	standalone := childIndex("events_p1", "events_p1_ts_standalone", "ts", 2001, 0)
	standalone.IdxScan = 10 // more used than the attached child
	snap.Indexes = append(snap.Indexes, standalone)
	got := ruleDuplicateIndexes(snap, nil, unusedCfg(), nil)
	if len(got) != 1 || got[0].ObjectIdentifier != "public.events_p1_ts_standalone" {
		t.Fatalf("findings = %v, want the standalone index dropped", identsOf(got))
	}
	if !strings.Contains(got[0].RecommendedSQL, `"events_p1_ts_standalone"`) {
		t.Errorf("SQL = %q, want the standalone drop", got[0].RecommendedSQL)
	}
}

func TestPartitionIndex_SubsetNeverDropsAttachedChild(t *testing.T) {
	snap := partitionedSnap(1)
	snap.Indexes = append(snap.Indexes,
		childIndex("events_p1", "events_p1_ts_id", "ts, id", 2001, 0))
	if got := ruleDuplicateIndexes(snap, nil, unusedCfg(), nil); len(got) != 0 {
		t.Fatalf("findings = %v, want none: the attached (ts) cannot be dropped",
			identsOf(got))
	}
	snap = partitionedSnap(1)
	snap.Indexes[0].IndexDef = strings.Replace(snap.Indexes[0].IndexDef,
		"(ts)", "(ts, id)", 1)
	snap.Indexes = append(snap.Indexes,
		childIndex("events_p1", "events_p1_ts_only", "ts", 2001, 0))
	got := ruleDuplicateIndexes(snap, nil, unusedCfg(), nil)
	if len(got) != 1 || got[0].ObjectIdentifier != "public.events_p1_ts_only" {
		t.Fatalf("findings = %v, want the standalone subset", identsOf(got))
	}
}
