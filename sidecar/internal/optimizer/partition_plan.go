package optimizer

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// maxIdentifierBytes is PostgreSQL's NAMEDATALEN - 1.
const maxIdentifierBytes = 63

// partitionPlan returns the statements that build an index on a
// partitioned table without blocking writes (Phase 0 item 7):
// CREATE INDEX ... ON ONLY the parent (an invalid, empty parent index),
// then for each partition CREATE INDEX CONCURRENTLY and ALTER INDEX ...
// ATTACH PARTITION; the parent index becomes valid once every partition
// is attached. PostgreSQL rejects CREATE INDEX CONCURRENTLY on the parent
// itself. Multi-level partitioning returns nil: a sub-partitioned child
// needs its own ON ONLY step, which is left to the operator.
func partitionPlan(spec IndexSpec, tc TableContext) []string {
	if tc.NestedPartitions || len(tc.PartitionChildren) == 0 {
		return nil
	}
	body := indexBody(spec)
	plan := []string{"CREATE INDEX IF NOT EXISTS " + sanitize.QuoteIdentifier(spec.Name) +
		" ON ONLY " + sanitize.QuoteQualifiedName(tc.Schema, tc.Table) + " " + body}
	parentIndex := sanitize.QuoteQualifiedName(tc.Schema, spec.Name)
	for _, child := range tc.PartitionChildren {
		schema, table, ok := strings.Cut(child, ".")
		if !ok {
			return nil
		}
		name := childIndexName(spec.Name, child)
		plan = append(plan,
			"CREATE INDEX CONCURRENTLY IF NOT EXISTS "+sanitize.QuoteIdentifier(name)+
				" ON "+sanitize.QuoteQualifiedName(schema, table)+" "+body,
			"ALTER INDEX "+parentIndex+" ATTACH PARTITION "+
				sanitize.QuoteQualifiedName(schema, name))
	}
	return plan
}

// indexBody is "USING method (keys) [INCLUDE (...)] [WHERE ...]".
func indexBody(spec IndexSpec) string {
	body := "USING " + spec.Method + " (" + spec.Keys + ")"
	if spec.Include != "" {
		body += " INCLUDE (" + spec.Include + ")"
	}
	if spec.Where != "" {
		body += " WHERE " + spec.Where
	}
	return body
}

// childIndexName derives a partition's index name from the parent index
// name and the partition, unique per partition and within 63 bytes.
func childIndexName(parent, child string) string {
	sum := sha256.Sum256([]byte(child))
	suffix := "_" + hex.EncodeToString(sum[:4])
	limit := maxIdentifierBytes - len(suffix)
	for len(parent) > limit { // never split a multi-byte character
		_, size := utf8.DecodeLastRuneInString(parent)
		parent = parent[:len(parent)-size]
	}
	return parent + suffix
}

// partitionChildren lists the direct partitions ("schema.table", sorted)
// of parentKey and reports whether any of them is itself partitioned.
func partitionChildren(snap *collector.Snapshot, parentKey string) ([]string, bool) {
	if snap == nil {
		return nil, false
	}
	parents := buildPartitionParentSet(snap)
	var children []string
	nested := false
	for _, p := range snap.Partitions {
		if p.ParentSchema+"."+p.ParentTable != parentKey {
			continue
		}
		child := p.ChildSchema + "." + p.ChildTable
		children = append(children, child)
		nested = nested || parents[child]
	}
	sort.Strings(children)
	return children, nested
}
