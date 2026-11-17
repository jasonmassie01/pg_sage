package schemadebt

import (
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

var placedAt = ColumnRef{Schema: "public", Table: "orders", Column: "placed_at"}

// readers is one top family reading public.orders.
func readers() []workpack.Family {
	return []workpack.Family{{ID: "workload/family/f1", Command: "SELECT",
		Relations: []string{"public.orders"}, Calls: 900, ExecMs: 4000}}
}

func timestampInputs() Inputs {
	f := serialFacts("public", "orders", "id")
	f.Column = serialFacts("public", "orders", "id").Column
	f.Column.Name, f.Column.Type, f.Column.NotNull = "placed_at", "timestamp without time zone",
		false
	f.Column.Default, f.Column.DefaultIsSeq, f.Column.SequenceName = "", false, ""
	f.Column.SequenceSchema, f.Column.SequenceType = "", ""
	return Inputs{Columns: map[ColumnRef]Column{placedAt: {Facts: f}}}
}

func bigintOrders(size int64) workpack.TableFact {
	t := ordersTable(size)
	t.Columns[0].Type = "bigint"
	return t
}

func TestBuild_TimestampWithoutTimeZoneOnATopQueryTableIsAPlannedSwap(t *testing.T) {
	cands := build(t, schemaPack(t, readers(), nil, bigintOrders(1<<20)), timestampInputs(),
		Options{})
	c := only(t, cands, KindTimestampNoTZ)
	if c.ActionType != candidate.ActionAdvice || c.Approval != candidate.ApprovalRequired {
		t.Fatalf("timestamp candidate %s / %s", c.ActionType, c.Approval)
	}
	if c.Target != "public.orders.placed_at" {
		t.Fatalf("target %q", c.Target)
	}
	for _, id := range []string{"schema/table/public.orders", "workload/family/f1"} {
		if !slices.Contains(c.EvidenceIDs, id) {
			t.Errorf("evidence %v lacks %s", c.EvidenceIDs, id)
		}
	}
	mustContain(t, "swap SQL", c.ForwardSQL,
		"timestamp without time zone -> timestamp with time zone")
	mustContain(t, "reason", c.Reason, "TimeZone")
}

func TestBuild_TimestampOnATableNoTopQueryReadsIsLeftAlone(t *testing.T) {
	fams := readers()
	fams[0].Relations = []string{"public.other"}
	cands := build(t, schemaPack(t, fams, nil, bigintOrders(1<<20)), timestampInputs(),
		Options{})
	if n := len(ofKind(cands, KindTimestampNoTZ)); n != 0 {
		t.Fatalf("%d timestamp items on an unread table", n)
	}
}

func TestBuild_TimestampWithoutFactsIsAdvice(t *testing.T) {
	cands := build(t, schemaPack(t, readers(), nil, bigintOrders(1<<20)), Inputs{}, Options{})
	c := only(t, cands, KindTimestampNoTZ)
	if c.ActionType != candidate.ActionAdvice || c.ForwardSQL != "" {
		t.Fatalf("timestamp without facts is %s with SQL %q, want advice", c.ActionType,
			c.ForwardSQL)
	}
}

// eventsTable is an unpartitioned public.events of size bytes whose
// created_at leads an index.
func eventsTable(size int64) workpack.TableFact {
	return workpack.TableFact{ID: "schema/table/public.events", Table: "public.events",
		Kind: "table", SizeBytes: size, Columns: []workpack.ColumnFact{
			{Name: "id", Type: "bigint"}, {Name: "created_at", Type: "timestamp with time zone"}},
		Constraints: []string{"events_pkey PRIMARY KEY (id)"},
		Indexes: []string{"CREATE UNIQUE INDEX events_pkey ON public.events USING btree (id)",
			"CREATE INDEX events_at ON public.events USING btree (created_at, id)"}}
}

func TestBuild_LargeTableBoundaryIsInclusive(t *testing.T) {
	opts := Options{LargeTableBytes: 1 << 30}
	for size, want := range map[int64]int{1 << 30: 1, 1<<30 - 1: 0, 1<<30 + 1: 1} {
		cands := build(t, schemaPack(t, nil, nil, eventsTable(size)), Inputs{}, opts)
		if got := len(ofKind(cands, KindLargeTable)); got != want {
			t.Errorf("size %d: %d large table items, want %d", size, got, want)
		}
	}
	c := only(t, build(t, schemaPack(t, nil, nil, eventsTable(1<<30)), Inputs{}, opts),
		KindLargeTable)
	if c.ActionType != candidate.ActionAdvice || c.Target != "public.events" {
		t.Fatalf("large table %s on %s, want advice", c.ActionType, c.Target)
	}
	mustContain(t, "reason", c.Reason, "created_at", "partition", "1073741824")
}

func TestBuild_LargeTableDefaultIs10GiB(t *testing.T) {
	if DefaultLargeTableBytes != 10<<30 {
		t.Fatalf("DefaultLargeTableBytes = %d, want 10 GiB", DefaultLargeTableBytes)
	}
	for size, want := range map[int64]int{10 << 30: 1, 10<<30 - 1: 0} {
		cands := build(t, schemaPack(t, nil, nil, eventsTable(size)), Inputs{}, Options{})
		if got := len(ofKind(cands, KindLargeTable)); got != want {
			t.Errorf("size %d with the default: %d items, want %d", size, got, want)
		}
	}
}

func TestBuild_LargeTableNeedsATimeOrderedKeyAndNoPartitions(t *testing.T) {
	part := eventsTable(20 << 30)
	part.Kind, part.PartitionKey, part.PartitionCount = "partitioned", "RANGE (created_at)", 12
	noTime := eventsTable(20 << 30)
	noTime.Indexes = noTime.Indexes[:1]
	for name, tab := range map[string]workpack.TableFact{"partitioned": part,
		"no time-ordered key": noTime} {
		if n := len(ofKind(build(t, schemaPack(t, nil, nil, tab), Inputs{}, Options{}),
			KindLargeTable)); n != 0 {
			t.Errorf("%s: %d large table items", name, n)
		}
	}
}

func TestBuild_NegativeLargeTableBytesIsAnError(t *testing.T) {
	_, _, err := Build(t.Context(), schemaPack(t, nil, nil), Inputs{}, env(),
		Options{LargeTableBytes: -1})
	if err == nil || !strings.Contains(err.Error(), "large_table_bytes") {
		t.Fatalf("err = %v, want one naming large_table_bytes", err)
	}
}

// churnTable is public.<name> with the extra column and the window's
// writes on its one index.
func churnTable(name, column string, w any) (workpack.TableFact, *workpack.Indexes) {
	tab := workpack.TableFact{ID: "schema/table/public." + name, Table: "public." + name,
		Kind: "table", Columns: []workpack.ColumnFact{{Name: "id", Type: "bigint"},
			{Name: column, Type: "text"}},
		Constraints: []string{name + "_pkey PRIMARY KEY (id)"}}
	idx := &workpack.Indexes{ScansStatus: "ok", Total: 1, Indexes: []workpack.IndexFact{{
		ID: "index/idx/public." + name + "_pkey", Schema: "public", Table: name,
		Index: name + "_pkey", Method: "btree", Valid: true, Primary: true, Unique: true,
		Definition: "CREATE UNIQUE INDEX " + name + "_pkey ON public." + name +
			" USING btree (id)", Scans: int64(10), TableWrites: w,
		DuplicateOf: []string{}, PrefixOf: []string{}}}}
	return tab, idx
}

func TestBuild_QueueAndSoftDeleteTablesWithChurn(t *testing.T) {
	churn := workpack.Writes{Inserts: 200000, Updates: 300000, Deletes: 150000}
	for kind, column := range map[string]string{KindQueueTable: "status",
		KindSoftDelete: "deleted_at"} {
		tab, idx := churnTable("jobs", column, churn)
		c := only(t, build(t, schemaPack(t, nil, idx, tab), Inputs{}, Options{}), kind)
		if c.ActionType != candidate.ActionAdvice || c.Target != "public.jobs" {
			t.Fatalf("%s: %s on %s, want advice", kind, c.ActionType, c.Target)
		}
		if !slices.Contains(c.EvidenceIDs, "index/idx/public.jobs_pkey") {
			t.Errorf("%s: evidence %v lacks the writes fact", kind, c.EvidenceIDs)
		}
		mustContain(t, kind+" reason", c.Reason, column, "450000")
	}
}

func TestBuild_LowChurnOrUnknownWritesIsNoQueueTable(t *testing.T) {
	cases := map[string]any{
		"low churn":      workpack.Writes{Inserts: 1000000, Updates: 10, Deletes: 0},
		"small table":    workpack.Writes{Inserts: 10, Updates: 30, Deletes: 20},
		"unknown writes": "unknown",
	}
	for name, w := range cases {
		tab, idx := churnTable("jobs", "status", w)
		cands := build(t, schemaPack(t, nil, idx, tab), Inputs{}, Options{})
		if n := len(ofKind(cands, KindQueueTable)); n != 0 {
			t.Errorf("%s: %d queue items", name, n)
		}
	}
}
