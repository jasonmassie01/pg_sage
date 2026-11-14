package optimizer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/workload"
)

// ErrPartitionsUnknown: the collector could not read the partition map
// this cycle, so whether a table is a partitioned parent or a partition is
// unknown; a context then would plan either as a standalone table, so none
// is built until the next cycle reads it (collection-05).
var ErrPartitionsUnknown = errors.New("partition map unavailable this cycle; " +
	"partition-sensitive recommendations wait for the next collection")

// TableContext builds the context of one table ("schema.table", quoted
// where needed) from the snapshot: its workload statements, indexes,
// write activity and partitions, plus bounded single-table catalog reads
// (columns, statistics of the queried columns, collation). It runs no
// EXPLAIN: the HypoPG what-if plans statements, bounded, when a candidate
// is measured. ok is false when the snapshot does not hold the table or
// the table belongs to a system or pg_sage schema.
func (o *Optimizer) TableContext(
	ctx context.Context, snap *collector.Snapshot, table string,
) (TableContext, bool, error) {
	if snap == nil {
		return TableContext{}, false, errors.New("table context: nil snapshot")
	}
	if !snap.Available("partitions") {
		return TableContext{}, false, fmt.Errorf("table context of %s: %w (%s)", table,
			ErrPartitionsUnknown, snap.Unavailable["partitions"])
	}
	ts, ok := findTable(snap, table)
	if !ok || skipSchema(ts.SchemaName) {
		return TableContext{}, false, nil
	}
	key := ts.SchemaName + "." + ts.RelName
	tableQueries := groupQueriesByTable(snap)
	rel := workload.Relation{Schema: ts.SchemaName, Name: ts.RelName}
	queries := filterByMinCalls(tableQueries[rel], int64(o.cfg.MinQueryCalls))
	isParent := buildPartitionParentSet(snap)[key]
	if isParent {
		queries = mergeChildQueries(queries, tableQueries,
			buildPartitionChildSet(snap), snap, key)
	}
	tc := baseContext(snap, ts, queries, isParent)
	o.readCatalog(ctx, &tc, queries)
	tc.Queries = GroupByFingerprint(tc.Queries)
	tc.JoinPairs = DetectJoinPairs(tc.Queries)
	return tc, true, nil
}

// baseContext is the part of a table's context the snapshot holds.
func baseContext(snap *collector.Snapshot, ts collector.TableStats, queries []QueryInfo,
	isParent bool) TableContext {
	tc := TableContext{
		Schema:         ts.SchemaName,
		Table:          ts.RelName,
		LiveTuples:     ts.NLiveTup,
		DeadTuples:     ts.NDeadTup,
		TableBytes:     ts.TableBytes,
		IndexBytes:     ts.IndexBytes,
		IndexCount:     countIndexes(snap.Indexes, ts.SchemaName, ts.RelName),
		Queries:        queries,
		Relpersistence: ts.Relpersistence,
		IsPartitioned:  isParent,
	}
	if isParent {
		tc.PartitionChildren, tc.NestedPartitions = partitionChildren(snap,
			ts.SchemaName+"."+ts.RelName)
	}
	tc.WriteRate = computeWriteRate(ts)
	tc.WriteRateKnown = writeRateKnown(ts)
	tc.Workload = classifyWorkload(tc.WriteRate, tc.LiveTuples)
	tc.Indexes = buildIndexInfo(snap.Indexes, ts.SchemaName, ts.RelName)
	return tc
}

// readCatalog adds the server's major version, the table's columns, their
// key defaults, column statistics and the database collation; without a
// database the context keeps none (and ColumnsKnown stays false).
func (o *Optimizer) readCatalog(ctx context.Context, tc *TableContext, queries []QueryInfo) {
	if o.pool == nil {
		return
	}
	o.fillCatalog(ctx, catalogread.New(o.pool, o.catalogTimeouts), tc, queries)
}

// fillCatalog runs readCatalog's reads on reader. A failed or empty column
// read leaves ColumnsKnown false: information_schema.columns lists only
// the columns the role may see, and a timeout is no evidence either.
func (o *Optimizer) fillCatalog(ctx context.Context, reader catalogread.Querier,
	tc *TableContext, queries []QueryInfo) {
	collation, major, err := fetchCollationAndMajor(ctx, reader)
	if err != nil {
		o.logFn("optimizer", "%v; version-specific index rules (PostgreSQL 18 skip scan) "+
			"are off for %s.%s this cycle", err, tc.Schema, tc.Table)
	}
	tc.Collation, tc.PGMajor = collation, major
	cols, err := fetchColumns(ctx, reader, tc.Schema, tc.Table)
	if err != nil {
		o.logFn("WARN", "optimizer: %v; column existence is not checked this cycle", err)
	}
	tc.Columns, tc.ColumnsKnown = cols, err == nil && len(cols) > 0
	defaults, err := fetchKeyDefaults(ctx, reader, tc.Schema, tc.Table)
	if err != nil {
		o.logFn("optimizer", "%v; explicit default opclasses and collations will not "+
			"match omitted ones", err)
	}
	tc.KeyDefaults = defaults
	tc.ColStats = fetchColStats(ctx, reader, tc.Schema, tc.Table, queries)
	if cols := leadingKeyColumns(*tc); len(cols) > 0 {
		tc.LeadingKeyStats = fetchNamedColStats(ctx, reader, tc.Schema, tc.Table, cols)
	}
}

// findTable is the snapshot's row for "schema.table"; an unqualified name
// means public, quoted parts keep their case.
func findTable(snap *collector.Snapshot, table string) (collector.TableStats, bool) {
	schema, name, ok := splitQualified(table)
	if !ok {
		return collector.TableStats{}, false
	}
	for _, ts := range snap.Tables {
		if ts.SchemaName == schema && ts.RelName == name {
			return ts, true
		}
	}
	return collector.TableStats{}, false
}

// splitQualified splits "schema.table" into its identifiers: quoted parts
// are taken as written, bare parts fold to lower case.
func splitQualified(s string) (schema, name string, ok bool) {
	parts, ok := identParts(strings.TrimSpace(s))
	switch {
	case !ok || len(parts) == 0 || len(parts) > 2:
		return "", "", false
	case len(parts) == 1:
		return "public", parts[0], true
	}
	return parts[0], parts[1], true
}

func identParts(s string) ([]string, bool) {
	var parts []string
	for s != "" {
		var part string
		if strings.HasPrefix(s, `"`) {
			end := strings.Index(s[1:], `"`)
			if end < 0 {
				return nil, false
			}
			part, s = s[1:end+1], s[end+2:]
		} else {
			end := strings.IndexByte(s, '.')
			if end < 0 {
				end = len(s)
			}
			part, s = strings.ToLower(s[:end]), s[end:]
		}
		if part == "" {
			return nil, false
		}
		parts = append(parts, part)
		if s == "" {
			break
		}
		if !strings.HasPrefix(s, ".") {
			return nil, false
		}
		s = s[1:]
	}
	return parts, true
}
