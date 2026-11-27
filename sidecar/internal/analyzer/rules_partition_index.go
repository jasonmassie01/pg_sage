package analyzer

import (
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// rules-03: an index attached to a partitioned index cannot be dropped on
// its own ("cannot drop index ... because index ... requires it"), so the
// unused_index rule never flags one. Instead the leaves are tallied per
// top-level partitioned index, and one advisory finding is raised on the
// parent when every leaf has been unused for the window.

// maxPartitionDepth bounds the walk up a partitioned index tree, so a
// malformed snapshot cannot loop it.
const maxPartitionDepth = 32

// partitionRollup tallies attached leaf indexes by the top-level
// partitioned index above them.
type partitionRollup struct {
	parents map[uint32]collector.PartitionedIndex
	tally   map[uint32]*rollupTally
}

type rollupTally struct {
	total, unused int
	// since is the latest leaf clock: when the last leaf became unused.
	since time.Time
}

func newPartitionRollup(snap *collector.Snapshot) *partitionRollup {
	r := &partitionRollup{
		parents: make(map[uint32]collector.PartitionedIndex, len(snap.PartitionedIndexes)),
		tally:   map[uint32]*rollupTally{},
	}
	for _, pi := range snap.PartitionedIndexes {
		r.parents[pi.IndexRelID] = pi
	}
	return r
}

// top returns the top-level partitioned index above oid; false when the
// tree is not in the snapshot.
func (r *partitionRollup) top(oid uint32) (collector.PartitionedIndex, bool) {
	for range maxPartitionDepth {
		pi, ok := r.parents[oid]
		if !ok {
			return collector.PartitionedIndex{}, false
		}
		if pi.ParentIndexOID == 0 {
			return pi, true
		}
		oid = pi.ParentIndexOID
	}
	return collector.PartitionedIndex{}, false
}

// note records one attached leaf: unused when it has been unused for the
// whole window since since. Standalone indexes are not tallied.
func (r *partitionRollup) note(idx collector.IndexStats, unused bool, since time.Time) {
	if idx.ParentIndexOID == 0 {
		return
	}
	top, ok := r.top(idx.ParentIndexOID)
	if !ok {
		return
	}
	t := r.tally[top.IndexRelID]
	if t == nil {
		t = &rollupTally{}
		r.tally[top.IndexRelID] = t
	}
	t.total++
	if unused {
		t.unused++
		if since.After(t.since) {
			t.since = since
		}
	}
}

// findings are the roll-ups whose every leaf is unused, by identifier.
func (r *partitionRollup) findings(windowDays int, risk StandbyRisk) []Finding {
	var out []Finding
	for oid, t := range r.tally {
		pi := r.parents[oid]
		if t.total == 0 || t.unused < t.total || !pi.IsValid || pi.IsUnique ||
			pi.IsPrimary || isSystemSchema(pi.SchemaName) {
			continue
		}
		out = append(out, WithStandbyGate(rollupFinding(pi, t, windowDays), risk))
	}
	sort.Slice(out, func(a, b int) bool {
		return out[a].ObjectIdentifier < out[b].ObjectIdentifier
	})
	return out
}

// rollupApproval is why a partitioned index drop is never automatic.
const rollupApproval = "dropping a partitioned index drops it on every partition, " +
	"cannot run CONCURRENTLY and locks the partitioned table and each partition"

func rollupFinding(pi collector.PartitionedIndex, t *rollupTally, windowDays int) Finding {
	ident := pi.SchemaName + "." + pi.IndexRelName
	drop := "DROP INDEX " + sanitize.QuoteQualifiedName(pi.SchemaName, pi.IndexRelName) + ";"
	usage := fmt.Sprintf("unused on %d of %d partitions", t.unused, t.total)
	return Finding{
		Category:         "unused_index",
		Severity:         "warning",
		ObjectType:       "index",
		ObjectIdentifier: ident,
		Title: fmt.Sprintf("Unused partitioned index %s (%s for %d+ days)",
			ident, usage, windowDays),
		Detail: map[string]any{
			"table":                pi.RelName,
			"partitioned_index":    true,
			"key_columns":          pi.KeyColumns,
			"partitions_unused":    t.unused,
			"partitions_total":     t.total,
			"usage_evidence":       "zero_scans",
			"unused_since":         t.since.UTC().Format(time.RFC3339),
			"manual_sql":           drop,
			DetailApprovalRequired: rollupApproval,
		},
		Recommendation: "This partitioned index is " + usage + ". Its partitions' " +
			"indexes cannot be dropped one by one; drop the parent by hand " +
			"(detail.manual_sql) with approval, in a quiet window: PostgreSQL " +
			"has no CONCURRENTLY for a partitioned index and the drop locks " +
			"every partition.",
		// Advisory only: no SQL is ever run for this finding.
		ActionRisk: "high_risk",
	}
}
