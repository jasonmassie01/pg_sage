package planhash

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// No concurrent access tests here: ComputeWith is a pure function of its
// input and resolver; concurrent catalog reads (Resolve) are exercised
// in partition_db_test.go.

// fakeRoots resolves "schema.rel" keys; a key mapped to an error fails.
type fakeRoots struct {
	roots map[string]Root
	errs  map[string]error
	calls int
}

func (f *fakeRoots) Root(schema, rel string) (Root, error) {
	f.calls++
	key := schema + "." + rel
	if err, ok := f.errs[key]; ok {
		return Root{}, err
	}
	if r, ok := f.roots[key]; ok {
		return r, nil
	}
	return Root{Schema: schema, Name: rel}, nil
}

// dailyRoots maps every events_pNNN partition (and its ts index) to the
// partitioned table public.events with the given leaf count.
func dailyRoots(leaves int, names ...string) *fakeRoots {
	f := &fakeRoots{roots: map[string]Root{}, errs: map[string]error{}}
	for _, n := range names {
		f.roots["public."+n] = Root{Schema: "public", Name: "events", Leaves: leaves}
		f.roots["public."+n+"_ts_idx"] = Root{Schema: "public", Name: "events_ts_idx"}
	}
	return f
}

func partitionNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("events_p%03d", i)
	}
	return out
}

func scanJSON(node, rel string) string {
	if node == "Index Scan" {
		return `{"Node Type": "Index Scan", "Parent Relationship": "Member",
		  "Scan Direction": "Forward", "Index Name": "` + rel + `_ts_idx",
		  "Relation Name": "` + rel + `", "Schema": "public", "Alias": "e"}`
	}
	return `{"Node Type": "` + node + `", "Parent Relationship": "Member",
	  "Relation Name": "` + rel + `", "Schema": "public", "Alias": "e"}`
}

// appendJSON builds an Append over one Seq Scan per partition name.
func appendJSON(kind string, removed int, names ...string) string {
	kids := make([]string, len(names))
	for i, n := range names {
		kids[i] = scanJSON("Seq Scan", n)
	}
	extra := ""
	if removed >= 0 {
		extra = fmt.Sprintf(`"Subplans Removed": %d, `, removed)
	}
	return `[{"Plan": {"Node Type": "` + kind + `", ` + extra +
		`"Plans": [` + strings.Join(kids, ",") + `]}}]`
}

func mustWith(t *testing.T, plan string, r RootResolver) string {
	t.Helper()
	h, err := ComputeWith([]byte(plan), Options{Roots: r})
	if err != nil {
		t.Fatalf("ComputeWith: %v", err)
	}
	return h
}

var resolvedPattern = regexp.MustCompile(`^v2:[0-9a-f]{32}$`)
var unresolvedPattern = regexp.MustCompile(`^v2u:[0-9a-f]{32}$`)

// A query pinned to one day by constants scans a different partition
// every day; resolved to its root the plan is the same plan.
func TestComputeWith_PartitionSwapHashesSame(t *testing.T) {
	day1 := `[{"Plan": ` + scanJSON("Seq Scan", "events_p20261105") + `}]`
	day2 := `[{"Plan": ` + scanJSON("Seq Scan", "events_p20261106") + `}]`
	roots := dailyRoots(30, "events_p20261105", "events_p20261106")
	h1, h2 := mustWith(t, day1, roots), mustWith(t, day2, roots)
	if h1 != h2 {
		t.Fatalf("daily partition swap changed the hash: %s != %s", h1, h2)
	}
	if !resolvedPattern.MatchString(h1) {
		t.Fatalf("resolved hash %q does not match %s", h1, resolvedPattern)
	}
	if roots.calls == 0 {
		t.Fatal("resolver was never consulted")
	}
	u1, u2 := mustCompute(t, day1), mustCompute(t, day2)
	if u1 == u2 {
		t.Fatal("unresolved hashes must keep partition names (nothing to map them by)")
	}
	if u1 == h1 || !unresolvedPattern.MatchString(u1) {
		t.Fatalf("unresolved hash %q must carry the v2u prefix, resolved %q", u1, h1)
	}
}

// Index scans name the partition's own index; it resolves to the
// partitioned index, so index scans do not flip daily either.
func TestComputeWith_PartitionIndexSwapHashesSame(t *testing.T) {
	day1 := `[{"Plan": ` + scanJSON("Index Scan", "events_p20261105") + `}]`
	day2 := `[{"Plan": ` + scanJSON("Index Scan", "events_p20261106") + `}]`
	roots := dailyRoots(30, "events_p20261105", "events_p20261106")
	if h1, h2 := mustWith(t, day1, roots), mustWith(t, day2, roots); h1 != h2 {
		t.Fatalf("partition index swap changed the hash: %s != %s", h1, h2)
	}
}

func TestComputeWith_ScanShapeChangeOnSamePartitionDiffers(t *testing.T) {
	seq := `[{"Plan": ` + scanJSON("Seq Scan", "events_p20261105") + `}]`
	idx := `[{"Plan": ` + scanJSON("Index Scan", "events_p20261105") + `}]`
	roots := dailyRoots(30, "events_p20261105")
	if mustWith(t, seq, roots) == mustWith(t, idx, roots) {
		t.Fatal("Index Scan and Seq Scan on one partition must differ")
	}
}

// Append children collapse to a set of shapes plus a count bucket:
// 1, 2-10, >10, or all (the resolver's leaf count).
func TestComputeWith_AppendChildBuckets(t *testing.T) {
	all := partitionNames(31)
	roots := dailyRoots(30, all...)
	hash := func(n int) string { return mustWith(t, appendJSON("Append", -1, all[:n]...), roots) }
	if hash(3) != hash(5) {
		t.Error("3 and 5 same-shape children are both in the 2-10 bucket")
	}
	if hash(2) != hash(10) {
		t.Error("2 and 10 children are both in the 2-10 bucket")
	}
	if hash(1) == hash(7) {
		t.Error("1 versus 7 children must differ")
	}
	if hash(10) == hash(11) {
		t.Error("10 versus 11 children must differ (documented bucket edge)")
	}
	if hash(11) != hash(29) {
		t.Error("11 and 29 children are both in the >10 bucket")
	}
	if hash(30) == hash(29) {
		t.Error("30 children of 30 leaves (all) must differ from 29 (>10)")
	}
	if hash(1) == hash(30) {
		t.Error("losing a partition prune (1 child becomes all) must change the hash")
	}
	// The table grew a partition: all of 31 is still "all".
	grown := dailyRoots(31, all...)
	if hash(30) != mustWith(t, appendJSON("Append", -1, all...), grown) {
		t.Error("scanning every leaf must hash the same as the leaf count grows")
	}
}

func TestComputeWith_AppendWithoutLeafCountNeverAll(t *testing.T) {
	names := partitionNames(12)
	unknown := dailyRoots(0, names...)
	if mustWith(t, appendJSON("Append", -1, names...), unknown) !=
		mustWith(t, appendJSON("Append", -1, names[:11]...), unknown) {
		t.Error("an unknown leaf count must fall back to the numeric bucket")
	}
}

func TestComputeWith_AppendChildOrderAndDuplicatesIgnored(t *testing.T) {
	roots := dailyRoots(30, "events_p1", "events_p2", "events_p3")
	mk := func(kids ...string) string {
		return `{"Node Type": "Append", "Plans": [` + strings.Join(kids, ",") + `]}`
	}
	a := mk(scanJSON("Seq Scan", "events_p1"), scanJSON("Index Scan", "events_p2"),
		scanJSON("Seq Scan", "events_p3"))
	b := mk(scanJSON("Index Scan", "events_p2"), scanJSON("Seq Scan", "events_p1"),
		scanJSON("Seq Scan", "events_p3"))
	if mustWith(t, a, roots) != mustWith(t, b, roots) {
		t.Error("Append child order must not change the hash")
	}
	onlySeq := mk(scanJSON("Seq Scan", "events_p1"), scanJSON("Seq Scan", "events_p2"),
		scanJSON("Seq Scan", "events_p3"))
	if mustWith(t, a, roots) == mustWith(t, onlySeq, roots) {
		t.Error("an Index Scan child turning into a Seq Scan must change the hash")
	}
}

func TestComputeWith_MergeAppendUsesChildSet(t *testing.T) {
	names := partitionNames(6)
	roots := dailyRoots(30, names...)
	if mustWith(t, appendJSON("Merge Append", -1, names[:3]...), roots) !=
		mustWith(t, appendJSON("Merge Append", -1, names[3:]...), roots) {
		t.Error("Merge Append over different partitions of one root must match")
	}
	if mustWith(t, appendJSON("Merge Append", -1, names[:3]...), roots) ==
		mustWith(t, appendJSON("Append", -1, names[:3]...), roots) {
		t.Error("Merge Append and Append must differ")
	}
}

func TestComputeWith_SubplansRemovedFlag(t *testing.T) {
	names := partitionNames(2)
	roots := dailyRoots(30, names...)
	pruned := mustWith(t, appendJSON("Append", 28, names...), roots)
	absent := mustWith(t, appendJSON("Append", -1, names...), roots)
	zero := mustWith(t, appendJSON("Append", 0, names...), roots)
	if pruned == absent {
		t.Error("Subplans Removed > 0 must be recorded")
	}
	if zero != absent {
		t.Error("Subplans Removed 0 is no pruning and must match an absent field")
	}
	if pruned != mustWith(t, appendJSON("Append", 5, names...), roots) {
		t.Error("the pruned flag must not encode how many subplans were removed")
	}
}

func TestComputeWith_UnresolvedRelationsTagHash(t *testing.T) {
	day := `[{"Plan": ` + scanJSON("Seq Scan", "events_p20261105") + `}]`
	failing := dailyRoots(30)
	failing.errs["public.events_p20261105"] = errors.New("catalog read timed out")
	ambiguous := dailyRoots(30)
	ambiguous.errs["public.events_p20261105"] = ErrUnresolved
	emptyName := &fakeRoots{roots: map[string]Root{"public.events_p20261105": {}}}
	cases := map[string]RootResolver{
		"nil resolver": nil, "resolver error": failing, "ambiguous": ambiguous,
		"empty root name": emptyName,
	}
	want := mustCompute(t, day)
	for name, r := range cases {
		h, err := ComputeWith([]byte(day), Options{Roots: r})
		if err != nil {
			t.Errorf("%s: unresolved relations must not fail the hash: %v", name, err)
			continue
		}
		if !unresolvedPattern.MatchString(h) || h != want {
			t.Errorf("%s: hash %q, want the unresolved %q", name, h, want)
		}
	}
}

// One unresolved relation is enough to make the fingerprint unresolved:
// a v2 hash claims every partition was mapped to its root.
func TestComputeWith_PartiallyResolvedIsUnresolved(t *testing.T) {
	plan := `{"Node Type": "Nested Loop", "Plans": [` +
		scanJSON("Seq Scan", "events_p1") + `,` + scanJSON("Seq Scan", "events_p2") + `]}`
	roots := dailyRoots(30, "events_p1", "events_p2")
	roots.errs["public.events_p2"] = ErrUnresolved
	if h := mustWith(t, plan, roots); !unresolvedPattern.MatchString(h) {
		t.Fatalf("partially resolved hash %q must carry v2u", h)
	}
}

func TestComputeWith_PlainTableResolvesToItself(t *testing.T) {
	roots := dailyRoots(30)
	h := mustWith(t, basePlan, roots)
	if !resolvedPattern.MatchString(h) {
		t.Fatalf("plain-table plan resolved by a resolver must be v2, got %q", h)
	}
	if h[len("v2:"):] != mustCompute(t, basePlan)[len("v2u:"):] {
		t.Error("resolving relations to themselves must not change the encoding")
	}
}

func TestComputeWith_InvalidPlansStillFail(t *testing.T) {
	roots := dailyRoots(30)
	for _, in := range []string{"", `{"foo": 1}`, `{"Node Type": "Append", "Plans": [1]}`,
		`{"Node Type": "Append", "Plans": [{"Node Type": ""}]}`, nestedPlan(MaxDepth)} {
		h, err := ComputeWith([]byte(in), Options{Roots: roots})
		if !errors.Is(err, ErrInvalidPlan) || h != "" {
			t.Errorf("input %.30q: hash %q err %v, want ErrInvalidPlan", in, h, err)
		}
	}
}

// Append children are encoded one level deeper than the Append, so the
// depth bound still applies under an Append.
func TestComputeWith_DepthBoundUnderAppend(t *testing.T) {
	deep := `{"Node Type": "Append", "Plans": [` + nestedPlan(MaxDepth-1) + `]}`
	if _, err := ComputeWith([]byte(deep), Options{}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("plan of depth %d under Append: want ErrInvalidPlan, got %v", MaxDepth+1, err)
	}
}

func TestRelations_CollectsDistinctTablesAndIndexes(t *testing.T) {
	root, err := rootNode([]byte(appendJSON("Append", -1, "events_p1", "events_p1")))
	if err != nil {
		t.Fatal(err)
	}
	got, err := relations(root)
	if err != nil {
		t.Fatalf("relations: %v", err)
	}
	if len(got) != 1 || got[0] != (Relation{Schema: "public", Name: "events_p1"}) {
		t.Fatalf("relations = %+v, want one public.events_p1", got)
	}
	idx, _ := rootNode([]byte(scanJSON("Index Scan", "events_p2")))
	got, err = relations(idx)
	if err != nil || len(got) != 2 || got[1].Name != "events_p2_ts_idx" {
		t.Fatalf("relations(index scan) = %+v, %v", got, err)
	}
	if _, err := relations(map[string]any{"Node Type": "Append", "Plans": "x"}); err == nil {
		t.Fatal("a malformed child list must fail")
	}
}

// More than MaxResolvedRelations names are refused before any read, so a
// huge plan cannot turn into an unbounded catalog query.
func TestResolve_TooManyRelationsFailsClosed(t *testing.T) {
	kids := make([]string, MaxResolvedRelations+1)
	for i := range kids {
		kids[i] = scanJSON("Seq Scan", fmt.Sprintf("t%d", i))
	}
	plan := []byte(`{"Node Type": "Append", "Plans": [` + strings.Join(kids, ",") + `]}`)
	roots, err := Resolve(context.Background(), nil, plan)
	if err == nil || !strings.Contains(err.Error(), "257 relations") {
		t.Fatalf("err = %v, want a too-many-relations error naming the count", err)
	}
	if h := mustWith(t, string(plan), roots); !unresolvedPattern.MatchString(h) {
		t.Fatalf("hash %q, want v2u", h)
	}
}

func TestResolve_NilDatabaseIsUnresolved(t *testing.T) {
	roots, err := Resolve(context.Background(), nil, []byte(basePlan))
	if err == nil || !strings.Contains(err.Error(), "no database") {
		t.Fatalf("err = %v, want a no-database error", err)
	}
	if h := mustWith(t, basePlan, roots); !unresolvedPattern.MatchString(h) {
		t.Fatalf("hash %q, want v2u", h)
	}
}

// A plan without relations (or an invalid one, which ComputeWith rejects
// anyway) needs no catalog read.
func TestResolve_NoRelationsNeedsNoRead(t *testing.T) {
	plan := []byte(`{"Node Type": "Result"}`)
	roots, err := Resolve(context.Background(), nil, plan, []byte("not json"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if h := mustWith(t, string(plan), roots); !resolvedPattern.MatchString(h) {
		t.Fatalf("hash %q, want v2", h)
	}
}
