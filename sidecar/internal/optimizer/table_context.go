package optimizer

import (
	"context"
	"errors"
	"strings"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
)

// TableContext builds the context of one table ("schema.table", quoted
// where needed) from the snapshot: its workload statements, indexes,
// write activity and partitions, plus bounded single-table catalog reads
// (columns, statistics of the queried columns, collation) and the plans of
// its own statements. ok is false when the snapshot does not hold the
// table or the table belongs to a system or pg_sage schema.
func (o *Optimizer) TableContext(
	ctx context.Context, snap *collector.Snapshot, table string,
) (TableContext, bool, error) {
	if snap == nil {
		return TableContext{}, false, errors.New("table context: nil snapshot")
	}
	ts, ok := findTable(snap, table)
	if !ok || skipSchema(ts.SchemaName) {
		return TableContext{}, false, nil
	}
	key := ts.SchemaName + "." + ts.RelName
	tableQueries := groupQueriesByTable(snap)
	queries := filterByMinCalls(tableQueries[key], int64(o.cfg.MinQueryCalls))
	isParent := buildPartitionParentSet(snap)[key]
	if isParent {
		queries = mergeChildQueries(queries, tableQueries,
			buildPartitionChildSet(snap), snap, key)
	}
	tc := baseContext(snap, ts, queries, isParent)
	o.readCatalog(ctx, &tc, queries)
	tc.Plans, tc.PlanSource = o.capturePlans(ctx, snap, queries)
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
		PlanSource:     "none",
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

// readCatalog adds the table's columns, column statistics and the
// database collation; without a database the context keeps none.
func (o *Optimizer) readCatalog(ctx context.Context, tc *TableContext, queries []QueryInfo) {
	if o.pool == nil {
		return
	}
	reader := catalogread.New(o.pool, o.catalogTimeouts)
	collation, err := fetchCollation(ctx, reader)
	if err != nil {
		o.logFn("optimizer", "read database collation: %v", err)
	}
	tc.Collation = collation
	tc.Columns = fetchColumns(ctx, reader, tc.Schema, tc.Table)
	tc.ColStats = fetchColStats(ctx, reader, tc.Schema, tc.Table, queries)
}

// capturePlans captures plans for the table's own statements only.
func (o *Optimizer) capturePlans(ctx context.Context, snap *collector.Snapshot,
	queries []QueryInfo) ([]PlanSummary, string) {
	if o.pool == nil || len(queries) == 0 {
		return nil, "none"
	}
	want := make(map[int64]bool, len(queries))
	for _, q := range queries {
		want[q.QueryID] = true
	}
	var stats []collector.QueryStats
	for _, q := range applicationQueries(snap.Queries) {
		if want[q.QueryID] {
			stats = append(stats, q)
		}
	}
	plans, source := o.planner.CapturePlans(ctx, stats)
	if len(plans) == 0 {
		return nil, "query_text_only"
	}
	return plans, source
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
