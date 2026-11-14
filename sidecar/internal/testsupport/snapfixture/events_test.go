package snapfixture_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/testsupport/snapfixture"
)

func withEvents(ev snapfixture.Events) snapfixture.Scenario {
	s := scenario()
	s.Events = ev
	return s
}

func scanSum(ixs []collector.IndexStats) int64 {
	var sum int64
	for _, ix := range ixs {
		sum += ix.IdxScan + ix.IdxTupRead + ix.IdxTupFetch
	}
	return sum
}

func tableCounters(tables []collector.TableStats) int64 {
	var sum int64
	for _, tb := range tables {
		sum += tb.SeqScan + tb.IdxScan + tb.NTupIns + tb.NTupUpd + tb.NTupDel
	}
	return sum
}

func TestCounterResetZeroesCountersAtItsCycle(t *testing.T) {
	cycles := generate(t, withEvents(snapfixture.Events{CounterReset: 8}))
	if scanSum(indexesAt(t, cycles, 7)) == 0 {
		t.Fatal("no index counter moved before the reset; the check below proves nothing")
	}
	if got := tableCounters(decode[collector.TableStats](t, cycles, 7, "tables")); got == 0 {
		t.Fatal("no table counter was set before the reset")
	}
	if got := scanSum(indexesAt(t, cycles, 8)); got != 0 {
		t.Errorf("index counters at the reset cycle = %d, want 0", got)
	}
	if got := tableCounters(decode[collector.TableStats](t, cycles, 8, "tables")); got != 0 {
		t.Errorf("table counters at the reset cycle = %d, want 0", got)
	}
}

// Without a reset, counters never go backwards, as pg_stat counters do not.
func TestCountersOnlyGrowWithoutAReset(t *testing.T) {
	cycles := generate(t, scenario())
	prev := map[string]int64{}
	for n := 1; n <= len(cycles); n++ {
		for _, ix := range indexesAt(t, cycles, n) {
			if ix.IdxScan < prev[ix.IndexRelName] {
				t.Errorf("cycle %d: %s idx_scan fell %d -> %d", n, ix.IndexRelName,
					prev[ix.IndexRelName], ix.IdxScan)
			}
			prev[ix.IndexRelName] = ix.IdxScan
		}
	}
}

func TestDropIndexDropsThenRecreatesTwoCyclesLater(t *testing.T) {
	const victim = "ix_t000_c00000"
	s := withEvents(snapfixture.Events{DropIndex: 3})
	cycles := generate(t, s)
	total := s.Tables + s.Indexes
	if ixs := indexesAt(t, cycles, 2); position(ixs, victim) != s.Tables || len(ixs) != total {
		t.Fatalf("before the drop: %s at %d of %d", victim, position(ixs, victim), len(ixs))
	}
	for _, n := range []int{3, 4} {
		if ixs := indexesAt(t, cycles, n); position(ixs, victim) != -1 || len(ixs) != total-1 {
			t.Errorf("cycle %d: %s at %d of %d, want dropped", n, victim,
				position(ixs, victim), len(ixs))
		}
	}
	ixs := indexesAt(t, cycles, 5)
	at := position(ixs, victim)
	if at != total-1 || len(ixs) != total {
		t.Fatalf("cycle 5: %s at %d of %d, want recreated last", victim, at, len(ixs))
	}
	again := ixs[at]
	if !strings.Contains(again.IndexDef, "(tenant_id, created_at)") || again.IdxScan != 0 {
		t.Errorf("recreated index = %+v, want a new definition and zero counters", again)
	}
}

func TestRedefineChangesTheSecondIndexUnderItsName(t *testing.T) {
	s := withEvents(snapfixture.Events{Redefine: 4})
	cycles := generate(t, s)
	before := indexesAt(t, cycles, 3)[s.Tables+1]
	if before.IndexRelName != "ix_t001_c00001" || before.IndexType != "btree" {
		t.Fatalf("before: %+v", before)
	}
	for _, n := range []int{4, 5} {
		after := indexesAt(t, cycles, n)[s.Tables+1]
		if after.IndexRelName != before.IndexRelName || after.IndexType != "hash" ||
			!strings.Contains(after.IndexDef, "USING hash (email)") {
			t.Errorf("cycle %d: %+v, want the same name redefined as hash", n, after)
		}
	}
}

func TestRenameRenamesTheThirdIndexInPlace(t *testing.T) {
	s := withEvents(snapfixture.Events{Rename: 4})
	cycles := generate(t, s)
	if got := indexesAt(t, cycles, 3)[s.Tables+2].IndexRelName; got != "ix_t002_c00002" {
		t.Fatalf("before: %s", got)
	}
	after := indexesAt(t, cycles, 4)[s.Tables+2]
	if after.IndexRelName != "ix_t002_c00002_renamed" ||
		!strings.Contains(after.IndexDef, "ix_t002_c00002_renamed") {
		t.Errorf("after: %+v", after)
	}
	if got := len(indexesAt(t, cycles, 4)); got != s.Tables+s.Indexes {
		t.Errorf("a rename changed the index count to %d", got)
	}
}

func TestEmptySpanWritesNullIndexes(t *testing.T) {
	s := withEvents(snapfixture.Events{EmptyFrom: 4, EmptyTo: 7})
	cycles := generate(t, s)
	for n := 1; n <= s.Cycles; n++ {
		data, ok := doc(t, cycles, n, "indexes")
		empty := n >= 4 && n < 7
		if !ok || (string(data) == "null") != empty {
			t.Errorf("cycle %d: indexes %.20s (present %v), empty %v", n, data, ok, empty)
		}
	}
	if got := len(indexesAt(t, cycles, 7)); got != s.Tables+s.Indexes {
		t.Errorf("indexes after the span = %d", got)
	}
}

func TestUnavailableCycleHasNoIndexesDocument(t *testing.T) {
	cycles := generate(t, withEvents(snapfixture.Events{Unavailable: 5}))
	for _, n := range []int{4, 5, 6} {
		_, ok := doc(t, cycles, n, "indexes")
		wantDocs := 9
		if n == 5 {
			wantDocs = 8
		}
		if ok == (n == 5) || len(cycles[n-1].Docs) != wantDocs {
			t.Errorf("cycle %d: indexes present %v, %d docs", n, ok, len(cycles[n-1].Docs))
		}
	}
}

func queryIDs(t *testing.T, cycles []snapfixture.Cycle, n int) map[int64]bool {
	t.Helper()
	ids := map[int64]bool{}
	for _, q := range decode[collector.QueryStats](t, cycles, n, "queries") {
		ids[q.QueryID] = true
	}
	return ids
}

func TestQueryChurnSwapsOneQueryEveryNCycles(t *testing.T) {
	cycles := generate(t, withEvents(snapfixture.Events{QueryChurn: 3}))
	for n := 2; n <= 12; n++ {
		prev, cur := queryIDs(t, cycles, n-1), queryIDs(t, cycles, n)
		var gone, added int
		for id := range prev {
			if !cur[id] {
				gone++
			}
		}
		for id := range cur {
			if !prev[id] {
				added++
			}
		}
		want := 0
		if n%3 == 0 {
			want = 1
		}
		if gone != want || added != want || len(cur) != 60 {
			t.Errorf("cycle %d: %d gone, %d added, %d queries; want %d swapped of 60",
				n, gone, added, len(cur), want)
		}
	}
}

// Boundary: index DDL needs more than three secondary indexes; with three
// the events are ignored rather than indexing out of range.
func TestIndexDDLNeedsFourSecondaryIndexes(t *testing.T) {
	ev := snapfixture.Events{DropIndex: 2, Redefine: 3, Rename: 4}
	small := withEvents(ev)
	small.Indexes = 3
	cycles := generate(t, small)
	first := names(indexesAt(t, cycles, 1))
	for n := 2; n <= small.Cycles; n++ {
		if got := names(indexesAt(t, cycles, n)); !reflect.DeepEqual(got, first) {
			t.Fatalf("cycle %d: indexes changed with three secondaries: %v", n, got)
		}
	}
	four := withEvents(snapfixture.Events{DropIndex: 2})
	four.Indexes = 4
	cycles = generate(t, four)
	if got := len(indexesAt(t, cycles, 2)); got != four.Tables+3 {
		t.Errorf("with four secondaries the drop left %d indexes, want %d", got, four.Tables+3)
	}
}

// Zero events mean never: the catalog keeps its names in every cycle.
func TestZeroEventsChangeNothing(t *testing.T) {
	cycles := generate(t, scenario())
	first := names(indexesAt(t, cycles, 1))
	for n := 2; n <= len(cycles); n++ {
		if got := names(indexesAt(t, cycles, n)); !reflect.DeepEqual(got, first) {
			t.Fatalf("cycle %d: index names changed without an event", n)
		}
	}
}
