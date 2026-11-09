package firstlook

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// No concurrent access tests here: the partition rules are pure functions
// over slices they do not retain or modify.

// partTree is a partitioned index (OID parentOID on table parentTable)
// and one attached leaf index per partition, with the given scan counts.
func partTree(parentOID, parentTable uint32, name string, scans ...int64) []Index {
	parent := btree(parentOID, parentTable, name, 2)
	parent.Table, parent.Partitioned, parent.SizeBytes = "p", true, 0
	out := []Index{parent}
	for i, s := range scans {
		n := uint32(i + 1)
		child := btree(parentOID*1000+n, parentTable*1000+n,
			fmt.Sprintf("p%d_%s", n, name), 2)
		child.Table = fmt.Sprintf("p%d", n)
		child.IsPartition, child.ParentOID, child.Scans = true, parentOID, s
		out = append(out, child)
	}
	return out
}

func noConcurrently(t *testing.T, it Item) {
	t.Helper()
	for field, s := range map[string]string{"SQL": it.SuggestedSQL, "caveat": it.Caveat,
		"recommendation": it.Recommendation} {
		if strings.Contains(s, "CONCURRENTLY") {
			t.Fatalf("%s item %s %s %q suggests CONCURRENTLY", it.Rule, it.Object, field, s)
		}
	}
}

var week = StatsWindow{Known: true, Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	Source: "stats_reset"}

func TestPartitionNeverScannedSumsChildren(t *testing.T) {
	now := week.Since.Add(30 * 24 * time.Hour)
	scanned := partTree(1, 10, "p_k", 0, 5, 0)
	if got := NeverScannedIndexes(scanned, nil, week, now); len(got) != 0 {
		t.Fatalf("a scanned partition: never-scanned %v, want none", rulesOf(got))
	}
	idle := partTree(1, 10, "p_k", 0, 0, 0)
	for i := range idle[1:] {
		idle[i+1].SizeBytes = 8192
	}
	got := NeverScannedIndexes(idle, nil, week, now)
	if len(got) != 1 || got[0].Object != "public.p_k" || got[0].Severity != SeverityWarning {
		t.Fatalf("never-scanned %v, want one warning for public.p_k", rulesOf(got))
	}
	it := got[0]
	requireEvidence(t, it)
	noConcurrently(t, it)
	if !strings.Contains(it.Detail, "3 partition indexes") ||
		!strings.Contains(it.Detail, "24 kB") {
		t.Fatalf("detail %q does not count the 3 partition indexes and their 24 kB", it.Detail)
	}
	if !strings.Contains(it.Caveat, "ACCESS EXCLUSIVE") ||
		!strings.Contains(it.Caveat, "Counters cover") {
		t.Fatalf("caveat %q lacks the window or the partition lock", it.Caveat)
	}
}

// A parent with no partitions has no scan sum: unknown, not zero.
func TestPartitionNeverScannedParentOnly(t *testing.T) {
	alone := partTree(1, 10, "p_k")
	if got := NeverScannedIndexes(alone, nil, week, time.Now()); len(got) != 0 {
		t.Fatalf("parent with no partitions: never-scanned %v, want none", rulesOf(got))
	}
	if got := NeverScannedIndexes(nil, nil, week, time.Now()); len(got) != 0 {
		t.Fatalf("nil indexes gave %v", got)
	}
}

// Sub-partitions: the top parent sums its leaves; the middle level and the
// leaves are never reported on their own.
func TestPartitionNeverScannedMultiLevel(t *testing.T) {
	tree := partTree(1, 10, "p_k", 0)
	mid := tree[1]
	mid.Partitioned = true
	tree[1] = mid
	leaf := btree(7, 70, "p1a_k", 2)
	leaf.IsPartition, leaf.ParentOID = true, mid.OID
	got := NeverScannedIndexes(append(tree, leaf), nil, week, time.Now())
	if len(got) != 1 || got[0].Object != "public.p_k" {
		t.Fatalf("idle tree: never-scanned %v, want only public.p_k", rulesOf(got))
	}
	leaf.Scans = 1
	if got := NeverScannedIndexes(append(tree, leaf), nil, week, time.Now()); len(got) != 0 {
		t.Fatalf("scanned leaf two levels down: never-scanned %v, want none", rulesOf(got))
	}
}

// An attached index whose parent was not read (the read stopped at its
// row bound) cannot be dropped on its own: no item, fail closed.
func TestPartitionOrphanChildNotReported(t *testing.T) {
	tree := partTree(1, 10, "p_k", 0, 0)[1:]
	items, flagged := DuplicateIndexes(tree)
	never := NeverScannedIndexes(tree, flagged, week, time.Now())
	if len(items) != 0 || len(never) != 0 {
		t.Fatalf("orphan children: %v %v, want nothing", rulesOf(items), rulesOf(never))
	}
}

// A detached-era index (no longer attached) is an ordinary index.
func TestPartitionDetachedChildIsOrdinary(t *testing.T) {
	x := btree(5, 50, "p9_k", 2)
	dup := btree(6, 50, "p9_k2", 2)
	items, flagged := DuplicateIndexes([]Index{x, dup})
	if len(items) != 1 || items[0].SuggestedSQL != "DROP INDEX CONCURRENTLY public.p9_k2;" ||
		items[0].Caveat != "" {
		t.Fatalf("detached duplicate: %+v", items)
	}
	never := NeverScannedIndexes([]Index{x}, flagged, week, time.Now())
	if len(never) != 1 || never[0].Object != "public.p9_k" ||
		strings.Contains(never[0].Caveat, "ACCESS EXCLUSIVE") {
		t.Fatalf("detached never-scanned: %+v", never)
	}
}

// Sixty partitions under two duplicate parents give one item, so the
// 50-item cap still holds 49 ordinary duplicates.
func TestPartitionDuplicatesOneItemUnderCap(t *testing.T) {
	scans := make([]int64, 60)
	// Both trees are on table 10, so their leaves share the 60 partitions.
	idx := append(partTree(1, 10, "p_k", scans...), partTree(2, 10, "p_k2", scans...)...)
	for n := uint32(0); n < 49; n++ {
		a, b := btree(5000+2*n, 900+n, fmt.Sprintf("t%d_a", n), 1),
			btree(5001+2*n, 900+n, fmt.Sprintf("t%d_b", n), 1)
		idx = append(idx, a, b)
	}
	items, flagged := DuplicateIndexes(idx)
	var part []Item
	for _, it := range items {
		if strings.HasPrefix(it.Object, "public.p") {
			part = append(part, it)
		}
	}
	if len(part) != 1 || part[0].Object != "public.p_k2" || !flagged[2] {
		t.Fatalf("partition duplicates: %v, want one for public.p_k2", rulesOf(part))
	}
	if part[0].SuggestedSQL != "DROP INDEX public.p_k2;" {
		t.Fatalf("partitioned duplicate SQL %q", part[0].SuggestedSQL)
	}
	noConcurrently(t, part[0])
	if !strings.Contains(part[0].Caveat, "ACCESS EXCLUSIVE") ||
		!strings.Contains(part[0].Caveat, "DETACH PARTITION") {
		t.Fatalf("caveat %q does not explain the lock and the per-partition path",
			part[0].Caveat)
	}
	kept, dropped := capItems(items, 50)
	if len(items) != 50 || dropped != 0 || len(kept) != 50 {
		t.Fatalf("%d items, %d dropped by the cap; want 50 and none", len(items), dropped)
	}
}

// A standalone index on a partition that copies an attached one: drop
// the standalone index, keep the attached one.
func TestPartitionStandaloneCopyOfAttached(t *testing.T) {
	tree := partTree(1, 10, "p_k", 0)
	stand := btree(3, tree[1].TableOID, "p1_extra", 2)
	stand.OID = 1 // lower OID than the attached copy: still not the keeper
	tree[0].OID = 9
	tree[1].ParentOID = 9
	items, _ := DuplicateIndexes(append(tree, stand))
	if len(items) != 1 || items[0].Object != "public.p1_extra" ||
		items[0].SuggestedSQL != "DROP INDEX CONCURRENTLY public.p1_extra;" {
		t.Fatalf("standalone copy: %+v, want the standalone dropped concurrently", items)
	}
}

func TestPartitionRedundantParentPrefix(t *testing.T) {
	short := partTree(1, 10, "p_k", 0, 0)
	long := partTree(2, 10, "p_k_id", 0, 0)
	for i := range long {
		long[i].Columns = []int16{2, 1}
		long[i].OpClasses = []uint32{1978, 1978}
		long[i].Collations = []uint32{0, 0}
	}
	items, flagged := DuplicateIndexes(append(short, long...))
	if len(items) != 1 || items[0].Rule != RuleRedundantIndex ||
		items[0].Object != "public.p_k" || !flagged[1] {
		t.Fatalf("redundant: %v, want one item for public.p_k", rulesOf(items))
	}
	if items[0].SuggestedSQL != "DROP INDEX public.p_k;" {
		t.Fatalf("redundant parent SQL %q", items[0].SuggestedSQL)
	}
	noConcurrently(t, items[0])
	if !strings.Contains(items[0].Caveat, "ACCESS EXCLUSIVE") {
		t.Fatalf("caveat %q", items[0].Caveat)
	}
}

func onOnlyParent() Index {
	x := btree(1, 10, "p_only", 1)
	x.Table, x.Partitioned, x.Valid = "p", true, false
	x.IndexDef = "CREATE INDEX p_only ON ONLY public.p USING btree (id)"
	x.UnattachedRead = true
	x.Unattached = []Partition{{Schema: "public", Name: "p2"},
		{Schema: "public", Name: "p3", Partitioned: true}}
	x.UnattachedTotal = 2
	return x
}

func TestPartitionInvalidOnOnlyNamesPartitions(t *testing.T) {
	items := InvalidIndexes([]Index{onOnlyParent()}, IndexBuilds{})
	if len(items) != 1 || items[0].Severity != SeverityWarning {
		t.Fatalf("invalid = %+v, want one warning", items)
	}
	it := items[0]
	requireEvidence(t, it)
	want := "CREATE INDEX CONCURRENTLY p2_p_only ON public.p2 USING btree (id);\n" +
		"ALTER INDEX public.p_only ATTACH PARTITION public.p2_p_only;\n" +
		"CREATE INDEX p3_p_only ON public.p3 USING btree (id);\n" +
		"ALTER INDEX public.p_only ATTACH PARTITION public.p3_p_only;"
	if it.SuggestedSQL != want {
		t.Fatalf("SQL\n%s\nwant\n%s", it.SuggestedSQL, want)
	}
	for _, s := range []string{it.SuggestedSQL, it.Detail, it.Recommendation} {
		if strings.Contains(s, "REINDEX") {
			t.Fatalf("partitioned invalid index suggests a REINDEX: %q", s)
		}
	}
	if !strings.Contains(it.Detail, "public.p2") || !strings.Contains(it.Detail, "public.p3") ||
		!strings.Contains(it.Detail, "2 partitions") {
		t.Fatalf("detail %q does not name the 2 unattached partitions", it.Detail)
	}
	if !strings.Contains(it.Recommendation, "blocks writes") {
		t.Fatalf("recommendation %q does not warn about the sub-partitioned build",
			it.Recommendation)
	}
}

func TestPartitionInvalidUniqueParent(t *testing.T) {
	x := onOnlyParent()
	x.Unique = true
	x.IndexDef = "CREATE UNIQUE INDEX p_only ON ONLY public.p USING btree (id)"
	x.Unattached = x.Unattached[:1]
	x.UnattachedTotal = 1
	it := InvalidIndexes([]Index{x}, IndexBuilds{})[0]
	if !strings.HasPrefix(it.SuggestedSQL,
		"CREATE UNIQUE INDEX CONCURRENTLY p2_p_only ON public.p2 USING btree (id);") {
		t.Fatalf("unique parent SQL %q", it.SuggestedSQL)
	}
	if strings.Contains(it.SuggestedSQL+it.Recommendation, "REINDEX") ||
		strings.Contains(it.SuggestedSQL, "SELECT") {
		t.Fatalf("unique parent took the unique-index path: %+v", it)
	}
}

// Every partition has an index attached, so one of those is invalid.
func TestPartitionInvalidAllAttached(t *testing.T) {
	x := onOnlyParent()
	x.Unattached, x.UnattachedTotal = nil, 0
	it := InvalidIndexes([]Index{x}, IndexBuilds{})[0]
	if it.SuggestedSQL != "" || !strings.Contains(it.Recommendation, "invalid") ||
		strings.Contains(it.Recommendation, "REINDEX") {
		t.Fatalf("all attached: %+v, want no SQL and a pointer to the invalid children", it)
	}
}

// The partition read failed or did not run: no SQL, no REINDEX.
func TestPartitionInvalidNotRead(t *testing.T) {
	x := onOnlyParent()
	x.UnattachedRead, x.Unattached, x.UnattachedTotal = false, nil, 0
	it := InvalidIndexes([]Index{x}, IndexBuilds{})[0]
	if it.SuggestedSQL != "" || strings.Contains(it.Recommendation, "REINDEX") ||
		!strings.Contains(it.Recommendation, "ATTACH PARTITION") {
		t.Fatalf("not read: %+v", it)
	}
}

// A definition that does not have the expected shape gives no SQL rather
// than a guessed one.
func TestPartitionInvalidUnexpectedDefinition(t *testing.T) {
	for _, def := range []string{"", "CREATE INDEX other ON ONLY public.p USING btree (id)",
		"CREATE INDEX p_only ON public.p USING btree (id)", "garbage"} {
		x := onOnlyParent()
		x.IndexDef = def
		it := InvalidIndexes([]Index{x}, IndexBuilds{})[0]
		if it.SuggestedSQL != "" || !strings.Contains(it.Detail, "public.p2") {
			t.Fatalf("definition %q: %+v, want no SQL and the partitions named", def, it)
		}
	}
}

// More partitions than the item lists: the count is the total, the SQL
// covers the listed ones.
func TestPartitionInvalidManyPartitions(t *testing.T) {
	x := onOnlyParent()
	x.Unattached = nil
	for i := 0; i < maxAttachListed; i++ {
		x.Unattached = append(x.Unattached, Partition{Schema: "public",
			Name: fmt.Sprintf("q%02d", i)})
	}
	x.UnattachedTotal = 75
	it := InvalidIndexes([]Index{x}, IndexBuilds{})[0]
	if got := strings.Count(it.SuggestedSQL, "ATTACH PARTITION"); got != maxAttachListed {
		t.Fatalf("%d ATTACH statements, want %d", got, maxAttachListed)
	}
	if !strings.Contains(it.Detail, "75 partitions") ||
		!strings.Contains(it.Recommendation, "55 more") {
		t.Fatalf("detail %q / recommendation %q do not give the total and the rest",
			it.Detail, it.Recommendation)
	}
}

func TestPartitionAttachNameBoundary(t *testing.T) {
	if got := attachName("p2", "p_only"); got != "p2_p_only" {
		t.Fatalf("attachName = %q", got)
	}
	exact := attachName(strings.Repeat("a", 30), strings.Repeat("b", 32))
	if len(exact) != 63 {
		t.Fatalf("63-byte name became %d bytes", len(exact))
	}
	long := attachName(strings.Repeat("a", 40), strings.Repeat("b", 40))
	if len(long) != 63 || !strings.HasPrefix(long, strings.Repeat("a", 40)+"_") {
		t.Fatalf("long name %q (%d bytes), want 63 bytes", long, len(long))
	}
	wide := attachName(strings.Repeat("é", 31), "x") // 62 bytes + "_x"
	if len(wide) > 63 || !utf8.ValidString(wide) {
		t.Fatalf("multibyte name %q (%d bytes) is over 63 bytes or cut mid-rune", wide,
			len(wide))
	}
}

// Ordinary invalid indexes keep today's wording and SQL.
func TestPartitionOrdinaryInvalidUnchanged(t *testing.T) {
	x := btree(2, 10, "t_bad", 2)
	x.Valid = false
	it := InvalidIndexes([]Index{x}, IndexBuilds{})[0]
	if it.SuggestedSQL != "REINDEX INDEX CONCURRENTLY public.t_bad;" ||
		!strings.Contains(it.Detail, "a failed CREATE INDEX CONCURRENTLY or REINDEX") {
		t.Fatalf("ordinary invalid index changed: %+v", it)
	}
}

// A foreign key on a partitioned table: no concurrent build exists for
// it, so the SQL builds without CONCURRENTLY and the caveat gives the
// per-partition path.
func TestPartitionUnindexedForeignKey(t *testing.T) {
	part := ForeignKey{Name: "fk_a", Schema: "public", Table: "child", TableOID: 10,
		Columns: []int16{1}, ColumnNames: []string{"a"}, RefTable: "public.parent",
		TableRows: -1, Partitioned: true}
	items := UnindexedForeignKeys([]ForeignKey{part}, nil, 0)
	if len(items) != 1 || items[0].SuggestedSQL != "CREATE INDEX ON public.child (a);" {
		t.Fatalf("partitioned foreign key: %+v", items)
	}
	noConcurrently(t, Item{Rule: items[0].Rule, SuggestedSQL: items[0].SuggestedSQL})
	if !strings.Contains(items[0].Caveat, "ON ONLY") ||
		!strings.Contains(items[0].Caveat, "ATTACH PARTITION") {
		t.Fatalf("caveat %q does not give the per-partition path", items[0].Caveat)
	}
	part.Partitioned = false
	plain := UnindexedForeignKeys([]ForeignKey{part}, nil, 0)
	if plain[0].SuggestedSQL != "CREATE INDEX CONCURRENTLY ON public.child (a);" ||
		plain[0].Caveat != "" {
		t.Fatalf("ordinary foreign key changed: %+v", plain[0])
	}
}
