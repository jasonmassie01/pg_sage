package analyzer

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// rules-03: PostgreSQL rejects CREATE INDEX CONCURRENTLY on a partitioned
// table, and its indexes (relkind I) are not in pg_stat_user_indexes. The
// missing_fk_index rule reads them from the snapshot's partitioned indexes
// and recommends the optimizer's non-blocking partition plan as advice.

// partitionLayout is the declarative partitioning of one snapshot.
type partitionLayout struct {
	snap     *collector.Snapshot
	relkind  map[tableKey]string
	parentOf map[tableKey]tableKey
	children map[tableKey]int
	keys     map[tableKey][][]string
}

func newPartitionLayout(snap *collector.Snapshot) *partitionLayout {
	l := &partitionLayout{snap: snap, relkind: map[tableKey]string{},
		parentOf: map[tableKey]tableKey{}, children: map[tableKey]int{},
		keys: buildFKRequirements(snap)}
	for _, t := range snap.Tables {
		l.relkind[tableKey{t.SchemaName, t.RelName}] = t.Relkind
	}
	for _, p := range snap.Partitions {
		parent := tableKey{p.ParentSchema, p.ParentTable}
		l.parentOf[tableKey{p.ChildSchema, p.ChildTable}] = parent
		l.children[parent]++
	}
	return l
}

// addParentIndexColumns adds each usable partitioned index's key columns
// to indexed: a valid, non-partial index on the parent covers the key on
// every partition.
func (l *partitionLayout) addParentIndexColumns(indexed map[tableKey][][]string) {
	for _, pi := range l.snap.PartitionedIndexes {
		if !pi.IsValid || pi.IsPartial || isSystemSchema(pi.SchemaName) {
			continue
		}
		key := tableKey{pi.SchemaName, pi.RelName}
		indexed[key] = append(indexed[key], pi.KeyColumns)
	}
}

// inheritsKey reports whether table is a partition whose partitioned
// parent has the same key: PostgreSQL clones a parent's foreign keys onto
// every partition, and the parent's finding covers them all.
func (l *partitionLayout) inheritsKey(table tableKey, cols []string) bool {
	parent, ok := l.parentOf[table]
	if !ok || l.relkind[parent] != "p" {
		return false
	}
	for _, have := range l.keys[parent] {
		if len(have) == len(cols) && isLeadingSet(cols, have) {
			return true
		}
	}
	return false
}

// partitionedFKFindings reports a key on partitioned table key that no
// partitioned index covers. Every leaf indexed: none. Some leaves indexed
// (indexes built per partition): each remaining leaf gets its own CREATE
// INDEX CONCURRENTLY, with detail.partition_of. No leaf indexed, or no
// partitions: one advisory finding on the parent (advisePartitionedFK).
// Partitions that could not be read give none: nothing is guessed.
func (l *partitionLayout) partitionedFKFindings(fk collector.ForeignKey, key tableKey,
	leaves []tableKey, indexed map[tableKey][][]string, unlogged map[string]bool,
) []Finding {
	if !l.snap.Available("partitions") {
		return nil
	}
	var missing []tableKey
	for _, leaf := range leaves {
		if !coveredBy(fk.Columns, indexed[leaf]) {
			missing = append(missing, leaf)
		}
	}
	if len(leaves) > 0 && len(missing) < len(leaves) {
		out := make([]Finding, 0, len(missing))
		for _, leaf := range missing {
			out = append(out, missingFKFinding(fk, key, leaf, unlogged))
		}
		return out
	}
	f := missingFKFinding(fk, key, key, unlogged)
	l.advisePartitionedFK(&f, key, fk)
	return []Finding{f}
}

// advisePartitionedFK makes a partitioned table's missing FK index
// advisory: no single statement builds it without blocking writes, so the
// plan (parent index ON ONLY, then each partition's index CONCURRENTLY and
// ATTACH PARTITION) is in detail.partition_plan.
func (l *partitionLayout) advisePartitionedFK(
	f *Finding, key tableKey, fk collector.ForeignKey,
) {
	f.RecommendedSQL, f.RollbackSQL = "", ""
	f.Detail["partitioned_parent"] = true
	quoted := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		quoted[i] = sanitize.QuoteIdentifier(c)
	}
	spec := optimizer.IndexSpec{Name: fkIndexName(fk.TableName, fk.Columns),
		Method: "btree", Keys: strings.Join(quoted, ", ")}
	note := " The table is partitioned, so the index cannot be built CONCURRENTLY " +
		"on the parent."
	plan := optimizer.PartitionPlan(l.snap, key.schema, key.table, spec)
	switch {
	case len(plan) > 0:
		f.Detail["partition_plan"] = plan
		note += " Run partition_plan in order: the parent index ON ONLY, then each " +
			"partition's index CONCURRENTLY and ATTACH PARTITION."
	case l.children[key] == 0:
		note += " It has no partitions yet: a plain CREATE INDEX on it is instant."
	default:
		note += " It has sub-partitioned partitions: build the index per leaf " +
			"partition and attach each level."
	}
	f.Recommendation += note
}

// fkIndexName names a partitioned table's FK index, within PostgreSQL's
// 63-byte identifier limit: a longer name is cut (never inside a
// character) and keeps a hash of the whole name, so cut names stay
// distinct.
func fkIndexName(table string, cols []string) string {
	const maxBytes = 63
	name := table + "_" + strings.Join(cols, "_") + "_fk_idx"
	if len(name) <= maxBytes {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "_" + hex.EncodeToString(sum[:4]) + "_fk_idx"
	for len(name) > maxBytes-len(suffix) {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name + suffix
}
