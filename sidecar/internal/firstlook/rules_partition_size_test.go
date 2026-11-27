package firstlook

import (
	"strings"
	"testing"
	"time"
)

// No concurrent access tests here: the rules are pure functions over
// slices they do not retain or modify.

// A partitioned index has no storage: its duplicate item states the total
// of its partitions' indexes, not "0 bytes".
func TestPartitionDuplicateParentStatesLeafSize(t *testing.T) {
	idx := append(partTree(1, 10, "p_k", 0, 0, 0), partTree(2, 10, "p_k2", 0, 0, 0)...)
	items, _ := DuplicateIndexes(idx)
	if len(items) != 1 || items[0].Object != "public.p_k2" {
		t.Fatalf("duplicates: %v, want one for public.p_k2", rulesOf(items))
	}
	if want := "public.p_k2 (240 kB) is identical"; !strings.Contains(items[0].Detail, want) {
		t.Fatalf("detail %q does not contain %q", items[0].Detail, want)
	}
}

func TestPartitionRedundantParentStatesLeafSize(t *testing.T) {
	short := partTree(1, 10, "p_k", 0, 0)
	long := partTree(2, 10, "p_k_id", 0, 0)
	for i := range long {
		long[i].Columns = []int16{2, 1}
		long[i].OpClasses = []uint32{1978, 1978}
		long[i].Collations = []uint32{0, 0}
	}
	items, _ := DuplicateIndexes(append(short, long...))
	if len(items) != 1 || !strings.Contains(items[0].Detail, "public.p_k (160 kB) lead") {
		t.Fatalf("redundant: %+v, want public.p_k at 160 kB", items)
	}
}

// An ordinary index keeps its own size in the item, byte for byte.
func TestPartitionOrdinaryDuplicateSizeUnchanged(t *testing.T) {
	items, _ := DuplicateIndexes([]Index{btree(1, 10, "a", 1), btree(2, 10, "b", 1)})
	if len(items) != 1 || !strings.Contains(items[0].Detail, "public.b (80 kB) is identical") {
		t.Fatalf("ordinary duplicate: %+v", items)
	}
}

// A parent with no partitions read has size 0 (nothing is known).
func TestPartitionTreeSizeWithoutLeaves(t *testing.T) {
	parent := partTree(1, 10, "p_k")
	if got := newPartitionTree(parent).size(parent[0]); got != 0 {
		t.Fatalf("size of a parent with no leaves = %d, want 0", got)
	}
}

// A read cut at its row limit may have left a parent's partitions out, so
// the parent's scan sum is unknown: no never-scanned item for it. An
// ordinary unscanned index is still reported.
func TestPartitionNeverScannedFailsClosedWhenReadCut(t *testing.T) {
	plain := btree(500, 50, "plain_a", 1)
	idx := append(partTree(1, 10, "p_k", 0, 0), plain)
	for _, cut := range []bool{false, true} {
		p := capPass(50)
		p.window = StatsWindow{Known: true, Since: time.Now().Add(-90 * 24 * time.Hour)}
		p.evalIndexes(idx, nil, IndexBuilds{}, cut)
		var got []string
		for _, it := range p.report.Items {
			if it.Rule == RuleNeverScannedIndex {
				got = append(got, it.Object)
			}
		}
		want := "public.p_k public.plain_a"
		if cut {
			want = "public.plain_a"
		}
		if strings.Join(got, " ") != want {
			t.Fatalf("read cut=%v: never-scanned %v, want %s", cut, got, want)
		}
	}
}

// The per-partition path drops the parent index before re-attaching:
// attaching a partition while the parent index exists rebuilds its index.
func TestPartitionDropCaveatOrder(t *testing.T) {
	c := partitionDropCaveat
	drop := strings.Index(c, "drop the parent index")
	attach := strings.Index(c, "then re-attach the partitions")
	if drop < 0 || attach < 0 || drop > attach {
		t.Fatalf("caveat %q must drop the parent index before re-attaching", c)
	}
	for _, want := range []string{"ACCESS EXCLUSIVE", "DETACH PARTITION", "lock_timeout",
		"not visible through the parent"} {
		if !strings.Contains(c, want) {
			t.Fatalf("caveat %q does not mention %q", c, want)
		}
	}
	if strings.Contains(c, "CONCURRENTLY") {
		t.Fatalf("caveat %q suggests CONCURRENTLY", c)
	}
}
