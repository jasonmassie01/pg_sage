package firstlook

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/sqlident"
)

// maxPartitionDepth bounds the walk down a partition tree.
const maxPartitionDepth = 64

// maxIdentBytes is NAMEDATALEN - 1: PostgreSQL truncates longer names.
const maxIdentBytes = 63

// partitionDropCaveat says what DROP INDEX on a partitioned index locks,
// and the per-partition path. Re-attaching a partition while the parent
// index exists rebuilds the index on it, so the parent goes before that.
const partitionDropCaveat = "This is a partitioned index: PostgreSQL cannot drop it " +
	"concurrently. DROP INDEX is quick once it runs, but it waits for and then holds an " +
	"ACCESS EXCLUSIVE lock on the parent and every partition until it commits, blocking " +
	"reads and writes on the whole table. Run it with a short lock_timeout and retry if " +
	"it times out. To lock one partition at a time instead, detach each partition " +
	"(ALTER TABLE ... DETACH PARTITION) and drop its now-standalone index, drop the parent " +
	"index once no partition is attached, then re-attach the partitions. A detached " +
	"partition's rows are not visible through the parent until it is re-attached."

// partitionBuildCaveat says what CREATE INDEX on a partitioned table
// locks, and the path that avoids it.
const partitionBuildCaveat = "The table is partitioned: PostgreSQL cannot build this " +
	"index concurrently, and CREATE INDEX blocks writes on every partition while it builds. " +
	"On a large tree, create it ON ONLY the parent, build it on each partition with " +
	"CREATE INDEX CONCURRENTLY, then attach each one with ALTER INDEX ... ATTACH PARTITION."

// partitionTree indexes the attached indexes by their parent.
type partitionTree struct {
	children map[uint32][]Index
}

func newPartitionTree(idx []Index) partitionTree {
	t := partitionTree{children: map[uint32][]Index{}}
	for _, x := range idx {
		if x.IsPartition && x.ParentOID != 0 {
			t.children[x.ParentOID] = append(t.children[x.ParentOID], x)
		}
	}
	return t
}

// leaves sums the scans and sizes of the leaf indexes under oid.
func (t partitionTree) leaves(oid uint32, depth int) (n int, scans, size int64) {
	if depth > maxPartitionDepth {
		return 0, 0, 0
	}
	for _, c := range t.children[oid] {
		if c.Partitioned {
			cn, cs, cz := t.leaves(c.OID, depth+1)
			n, scans, size = n+cn, scans+cs, size+cz
			continue
		}
		n, scans, size = n+1, scans+c.Scans, size+c.SizeBytes
	}
	return n, scans, size
}

// size is x's size; for a partitioned index, which has no storage of its
// own, the total of its leaf indexes.
func (t partitionTree) size(x Index) int64 {
	if !x.Partitioned {
		return x.SizeBytes
	}
	_, _, size := t.leaves(x.OID, 0)
	return size
}

// withoutPartitioned leaves out the partitioned indexes. After a read cut
// at its row limit, a partitioned index's partitions may lie past the cut,
// so the sum of their scans is unknown and it is not reported (fail closed).
func withoutPartitioned(idx []Index) []Index {
	out := make([]Index, 0, len(idx))
	for _, x := range idx {
		if !x.Partitioned {
			out = append(out, x)
		}
	}
	return out
}

// neverScannedParent reports a top-level partitioned index whose leaf
// indexes have no scans in total; ok is false when it has no leaves, so
// the sum is unknown, or when they were scanned.
func neverScannedParent(x Index, t partitionTree, w StatsWindow, sev string,
	now time.Time) (Item, bool) {
	n, scans, size := t.leaves(x.OID, 0)
	if n == 0 || scans != 0 {
		return Item{}, false
	}
	return Item{Rule: RuleNeverScannedIndex, Severity: sev,
		Object: x.object(), Title: "Index " + x.object() + " was never scanned",
		Detail: fmt.Sprintf("%s on %s is partitioned: its %d partition indexes (%s) have "+
			"idx_scan = 0 in total.", x.object(), qualified(x.Schema, x.Table), n,
			sizeText(size)),
		Recommendation: "Confirm no replica or rare job uses it, then consider dropping it. " +
			"pg_sage's unused-index rule keeps watching it.",
		Caveat: windowCaveat(w, now) + " " + partitionDropCaveat,
		Evidence: []Evidence{{Source: "pg_stat_user_indexes",
			Ref: fmt.Sprintf("pg_stat_user_indexes.idx_scan summed over the %d partition "+
				"indexes of %s = 0", n, x.object())}}}, true
}

// attachName names the index to create on partition for the partitioned
// index parent: "<partition>_<parent>", cut to 63 bytes on a rune boundary.
func attachName(partition, parent string) string {
	name := partition + "_" + parent
	for len(name) > maxIdentBytes {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// indexSpec is the part of x's definition after "ON ONLY <table> ":
// method, columns, INCLUDE and predicate. "" when the definition does not
// have that shape, so no SQL is guessed.
func indexSpec(x Index) string {
	head := "INDEX " + sqlident.Ident(x.Name) + " ON ONLY " +
		sqlident.Qualified(x.Schema, x.Table) + " "
	for _, create := range []string{"CREATE ", "CREATE UNIQUE "} {
		if spec, ok := strings.CutPrefix(x.IndexDef, create+head); ok &&
			strings.HasPrefix(spec, "USING ") {
			return spec
		}
	}
	return ""
}

// attachSQL creates x's index on each listed partition and attaches it. A
// partition that is partitioned itself cannot build concurrently.
func attachSQL(x Index) string {
	spec := indexSpec(x)
	if spec == "" {
		return ""
	}
	create := "CREATE INDEX "
	if x.Unique {
		create = "CREATE UNIQUE INDEX "
	}
	var b strings.Builder
	for _, p := range x.Unattached {
		name := attachName(p.Name, x.Name)
		c := create + "CONCURRENTLY "
		if p.Partitioned {
			c = create
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s%s ON %s %s;\nALTER INDEX %s ATTACH PARTITION %s;", c,
			sqlident.Ident(name), sqlident.Qualified(p.Schema, p.Name), spec,
			sqlident.Qualified(x.Schema, x.Name), sqlident.Qualified(p.Schema, name))
	}
	return b.String()
}

// partitionedInvalidText fills the item of an invalid partitioned index:
// created ON ONLY the parent, it stays invalid until every partition has
// a valid index attached. A REINDEX does not attach anything.
func partitionedInvalidText(it *Item, x Index, on string) {
	it.Detail = "Partitioned index " + on + ": it stays invalid until every partition " +
		"has a valid index attached to it, as after CREATE INDEX ... ON ONLY."
	switch {
	case !x.UnattachedRead:
		it.Recommendation = "Find the partitions with no index attached to it, create the " +
			"index on each (concurrently where it is a plain table) and attach it with " +
			"ALTER INDEX " + sqlident.Qualified(x.Schema, x.Name) + " ATTACH PARTITION."
		return
	case x.UnattachedTotal == 0:
		it.Detail += " Every partition has an index attached, so one of them is invalid."
		it.Recommendation = "Fix the invalid partition indexes this rule lists; this " +
			"index becomes valid once all of them are."
		return
	}
	names := make([]string, len(x.Unattached))
	sub := false
	for i, p := range x.Unattached {
		names[i] = qualified(p.Schema, p.Name)
		sub = sub || p.Partitioned
	}
	it.Detail += fmt.Sprintf(" %d partitions have none: %s.", x.UnattachedTotal,
		strings.Join(names, ", "))
	it.Recommendation = "Create the index on each partition without one, then attach it."
	if rest := x.UnattachedTotal - len(x.Unattached); rest > 0 {
		it.Recommendation += fmt.Sprintf(" The SQL covers the first %d; repeat it for the "+
			"%d more.", len(x.Unattached), rest)
	}
	if sub {
		it.Recommendation += " A partition that is partitioned itself cannot build " +
			"concurrently: its CREATE INDEX blocks writes on it while it builds."
	}
	it.SuggestedSQL = attachSQL(x)
}
