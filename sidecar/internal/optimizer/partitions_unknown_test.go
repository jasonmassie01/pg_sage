package optimizer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// collection-05: when the collector could not read pg_inherits this cycle
// the partition map is unknown, not empty. Building a table context then
// would treat a partitioned parent or a partition as a standalone table,
// so the context is withheld for the cycle.
//
// No concurrent access tests: TableContext reads only the snapshot it is
// handed and the optimizer's immutable configuration.

func partitionSnapshot() *collector.Snapshot {
	return &collector.Snapshot{
		Queries: []collector.QueryStats{{QueryID: 1,
			Query: "SELECT * FROM app.events WHERE kind = $1", Calls: 50,
			TotalExecTime: 500, MeanExecTime: 10}},
		Tables: []collector.TableStats{
			{SchemaName: "app", RelName: "events", NLiveTup: 1000, Relkind: "p"},
			{SchemaName: "app", RelName: "events_2026", NLiveTup: 1000, Relkind: "r"},
		},
		Partitions: []collector.PartitionInfo{{ChildSchema: "app", ChildTable: "events_2026",
			ParentSchema: "app", ParentTable: "events"}},
	}
}

// Happy path: partitions known, the parent's context names its partition.
func TestTableContext_KnownPartitionsBuildTheParentContext(t *testing.T) {
	o := New(nil, admissionConfig(), admitNoLog)
	tc, ok, err := o.TableContext(context.Background(), partitionSnapshot(), "app.events")
	if err != nil || !ok {
		t.Fatalf("context: ok %v err %v", ok, err)
	}
	if !tc.IsPartitioned || strings.Join(tc.PartitionChildren, ",") != "app.events_2026" {
		t.Fatalf("parent context = partitioned %v children %v", tc.IsPartitioned,
			tc.PartitionChildren)
	}
}

// Error propagation: partitions unknown, no context for the parent or for
// a partition (either would be planned as a standalone table), and the
// error says why.
func TestTableContext_UnknownPartitionsWithholdTheContext(t *testing.T) {
	o := New(nil, admissionConfig(), admitNoLog)
	snap := partitionSnapshot()
	snap.Partitions = nil
	snap.Unavailable = map[string]string{"partitions": "canceling statement due to " +
		"statement timeout"}
	for _, table := range []string{"app.events", "app.events_2026"} {
		tc, ok, err := o.TableContext(context.Background(), snap, table)
		if ok || !errors.Is(err, ErrPartitionsUnknown) {
			t.Fatalf("%s: ok %v err %v, want ErrPartitionsUnknown", table, ok, err)
		}
		if !strings.Contains(err.Error(), "statement timeout") || tc.Table != "" {
			t.Fatalf("%s: err %q / context %+v, want the collector's reason and no context",
				table, err, tc)
		}
	}
}

// Nil/empty: no partitions read as none (an available empty list) is a
// real fact, and a plain table gets its context.
func TestTableContext_NoPartitionsIsAFact(t *testing.T) {
	o := New(nil, admissionConfig(), admitNoLog)
	snap := partitionSnapshot()
	snap.Partitions = nil
	tc, ok, err := o.TableContext(context.Background(), snap, "app.events_2026")
	if err != nil || !ok || tc.IsPartitioned || tc.Table != "events_2026" {
		t.Fatalf("plain table: ok %v err %v context %+v", ok, err, tc)
	}
}
