package optimizer

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/collector"
)

// indexes-02: the write rate divided written tuples by scans plus written
// tuples, so one bulk COPY outweighed millions of key lookups. It is now
// index-maintaining writes (inserts and non-HOT updates) over those plus
// the tuples read, in percent.

func TestComputeWriteRate_TuplesNotScans(t *testing.T) {
	for _, c := range []struct {
		name string
		ts   collector.TableStats
		want float64
	}{
		{"3M inserts vs 10M index tuples fetched", collector.TableStats{NTupIns: 3_000_000,
			IdxScan: 1_000_000, IdxTupFetch: 10_000_000}, 100 * 3.0 / 13.0},
		{"HOT updates maintain no index", collector.TableStats{NTupUpd: 1000,
			NTupHotUpd: 1000, SeqTupRead: 1000}, 0},
		{"non-HOT updates do", collector.TableStats{NTupUpd: 1000, NTupHotUpd: 250,
			SeqTupRead: 250}, 75},
		{"deletes maintain no index until vacuum", collector.TableStats{NTupDel: 500,
			IdxTupFetch: 500}, 0},
		{"sequential tuples read count as reads", collector.TableStats{NTupIns: 100,
			SeqScan: 1, SeqTupRead: 300}, 25},
		{"scans without tuples are not reads", collector.TableStats{NTupIns: 10,
			SeqScan: 1000, IdxScan: 1000}, 100},
		{"more HOT than updates (counter skew) is no negative write",
			collector.TableStats{NTupUpd: 5, NTupHotUpd: 9, SeqTupRead: 10}, 0},
		{"nothing recorded", collector.TableStats{}, 0},
	} {
		if got := computeWriteRate(c.ts); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: write rate %.4f, want %.4f", c.name, got, c.want)
		}
	}
}

// Units: the COPY-loaded, key-read table of the plan reads as 23%
// (oltp_read), not 75% (oltp_write).
func TestBaseContext_CopyLoadedKeyReadTableIsNotWriteHeavy(t *testing.T) {
	ts := collector.TableStats{SchemaName: "public", RelName: "events", NLiveTup: 3_000_000,
		NTupIns: 3_000_000, IdxScan: 1_000_000, IdxTupFetch: 10_000_000}
	tc := baseContext(&collector.Snapshot{Tables: []collector.TableStats{ts}}, ts, nil, false)
	if math.Round(tc.WriteRate) != 23 || tc.Workload != "oltp_read" || !tc.WriteRateKnown {
		t.Fatalf("write rate %.2f workload %q known %t, want 23%% oltp_read", tc.WriteRate,
			tc.Workload, tc.WriteRateKnown)
	}
}

func TestClassifyWorkload_WriteHeavyBoundary(t *testing.T) {
	if got := classifyWorkload(70.00, 1000); got == "oltp_write" {
		t.Fatalf("70.00%% classified %q, want not write-heavy", got)
	}
	if got := classifyWorkload(70.01, 1000); got != "oltp_write" {
		t.Fatalf("70.01%% classified %q, want oltp_write", got)
	}
}

// --- max indexes per table ---

func capTable() TableContext {
	tc := sampleTableContext()
	tc.ColumnsKnown = true
	tc.IndexCount = 10
	tc.Indexes = append(tc.Indexes, IndexInfo{Name: "orders_status_old", OID: 4242,
		IsValid: true, Definition: "CREATE INDEX orders_status_old ON public.orders " +
			"USING btree (status, id)"})
	return tc
}

const capDDL = "CREATE INDEX CONCURRENTLY orders_status_created ON public.orders " +
	"(status, created_at)"

// Fails before the fix: the index the replacement removes was counted.
func TestCheckMaxIndexes_ReplacementAtTheCapIsAdmitted(t *testing.T) {
	v := fnNewTestValidator(fnTestOptimizerConfig())
	rec := Recommendation{DDL: capDDL, IndexType: "btree",
		Replaces: &IndexRef{Name: "orders_status_old", OID: 4242}}
	if ok, reason := v.checkMaxIndexes(rec, capTable()); !ok {
		t.Fatalf("replacement at the cap rejected: %s", reason)
	}
}

func TestCheckMaxIndexes_NewIndexAtTheCapIsRejected(t *testing.T) {
	v := fnNewTestValidator(fnTestOptimizerConfig())
	for name, ref := range map[string]*IndexRef{
		"no replacement":            nil,
		"replaces an unknown index": {Name: "no_such_index", OID: 1},
		"same name, another object": {Name: "orders_status_old", OID: 9999},
		"empty reference":           {},
	} {
		rec := Recommendation{DDL: capDDL, IndexType: "btree", Replaces: ref}
		ok, reason := v.checkMaxIndexes(rec, capTable())
		if ok || reason != "table already has maximum indexes" {
			t.Errorf("%s: ok=%t reason %q, want the cap", name, ok, reason)
		}
	}
}

func TestCheckMaxIndexes_ReplacingAnInvalidIndexFreesNoSlot(t *testing.T) {
	v := fnNewTestValidator(fnTestOptimizerConfig())
	tc := capTable()
	tc.Indexes[len(tc.Indexes)-1].IsValid = false
	rec := Recommendation{DDL: capDDL, Replaces: &IndexRef{Name: "orders_status_old",
		OID: 4242}}
	if ok, _ := v.checkMaxIndexes(rec, tc); ok {
		t.Fatal("an invalid index is not counted, so replacing it frees no slot")
	}
}

func TestCheckMaxIndexes_BelowTheCap(t *testing.T) {
	v := fnNewTestValidator(fnTestOptimizerConfig())
	tc := capTable()
	tc.IndexCount = 9
	if ok, reason := v.checkMaxIndexes(Recommendation{DDL: capDDL}, tc); !ok {
		t.Fatalf("9 of 10: %s", reason)
	}
}

func TestCountIndexes_InvalidIndexesAreNotCounted(t *testing.T) {
	ix := func(name string, valid bool, oid uint32) collector.IndexStats {
		return collector.IndexStats{SchemaName: "public", RelName: "orders",
			IndexRelName: name, IsValid: valid, IndexRelID: oid,
			IndexDef: "CREATE INDEX " + name + " ON public.orders USING btree (id)"}
	}
	snap := &collector.Snapshot{Indexes: []collector.IndexStats{ix("a", true, 11),
		ix("b_ccnew", false, 12), ix("c", true, 13),
		{SchemaName: "public", RelName: "other", IndexRelName: "d", IsValid: true}}}
	ts := collector.TableStats{SchemaName: "public", RelName: "orders"}
	tc := baseContext(snap, ts, nil, false)
	if tc.IndexCount != 2 || len(tc.Indexes) != 3 {
		t.Fatalf("count %d of %d indexes, want 2 valid of 3", tc.IndexCount, len(tc.Indexes))
	}
	if tc.Indexes[0].OID != 11 || tc.Indexes[2].OID != 13 {
		t.Fatalf("index identities = %+v, want the snapshot's OIDs", tc.Indexes)
	}
}

func TestAdmit_ReplacementAtTheCapReachesTheWhatIf(t *testing.T) {
	o, w, _ := admissionOptimizer(newMemStore(), measured(30))
	o.cfg.MaxIndexesPerTable = 2
	tc := admitTable()
	tc.IndexCount = 2
	tc.Indexes = []IndexInfo{{Name: "ai_claims_pkey", IsValid: true, IsUnique: true,
		OID: 1, Definition: "CREATE UNIQUE INDEX ai_claims_pkey ON public.ai_claims " +
			"USING btree (id)"}, {Name: "ai_claims_status_id", IsValid: true, OID: 2,
		Definition: "CREATE INDEX ai_claims_status_id ON public.ai_claims USING btree " +
			"(status, id)"}}
	rec := candidate("CREATE INDEX CONCURRENTLY ai_claims_status_ev ON public.ai_claims " +
		"(status, evidence_event_ids_json)")
	if a := o.Admit(context.Background(), rec, tc); a.Outcome != AdmitInvalid ||
		a.Reason != "table already has maximum indexes" {
		t.Fatalf("new index at the cap: %+v", a)
	}
	rec.Replaces = &IndexRef{Name: "ai_claims_status_id", OID: 2}
	a := o.Admit(context.Background(), rec, tc)
	if a.Outcome != AdmitAccepted || w.calls.Load() != 1 || a.Rec.Replaces == nil ||
		a.Rec.Replaces.OID != 2 {
		t.Fatalf("replacement at the cap: %+v (what-ifs %d)", a, w.calls.Load())
	}
}

// --- column metadata errors ---

// failingQuerier fails every catalog read the way a statement timeout does.
type failingQuerier struct{ err error }

func (f failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, f.err
}

func (f failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return errRow{f.err}
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

var errStatementTimeout = errors.New(
	"ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")

func TestFetchColumns_ReturnsTheQueryError(t *testing.T) {
	cols, err := fetchColumns(context.Background(), failingQuerier{errStatementTimeout},
		"public", "orders")
	if cols != nil || !errors.Is(err, errStatementTimeout) {
		t.Fatalf("columns %v err %v, want no columns and the query error", cols, err)
	}
	if !strings.Contains(err.Error(), "public.orders") {
		t.Fatalf("error %q does not name the table", err)
	}
}

// Error propagation: a failed column read is logged with the table, and the
// context says the column list is unknown.
func TestFillCatalog_ColumnReadErrorIsLogged(t *testing.T) {
	logs := &logRecorder{}
	o := New(nil, admissionConfig(), logs.log)
	tc := TableContext{Schema: "public", Table: "orders"}
	o.fillCatalog(context.Background(), failingQuerier{errStatementTimeout}, &tc, nil)
	if tc.ColumnsKnown || tc.Columns != nil {
		t.Fatalf("columns known %t (%v) after a failed read", tc.ColumnsKnown, tc.Columns)
	}
	if got := logs.matching("read columns of public.orders", "statement timeout"); len(got) != 1 {
		t.Fatalf("column read error logs = %q, want one naming the table", logs.lines)
	}
}

// No state-transition tests for the write rate: it is a pure function of
// one snapshot's counters.
