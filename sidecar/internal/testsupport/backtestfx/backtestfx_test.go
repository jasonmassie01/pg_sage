package backtestfx

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The fixture writer's own tests (T5 round 2: CI's coverage floor counts
// each package's own tests, and verify/backtest's use of this package did
// not count for it). No concurrent access tests: a fixture is written by
// one test at a time and Persist keeps no state between calls.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/testsupport/backtestfx"))
}

// fatalTB records the first Fatal and stops the calling goroutine, as
// testing.T does.
type fatalTB struct {
	testing.TB
	msg string
}

func (f *fatalTB) Helper() {}

func (f *fatalTB) Fatal(args ...any) {
	f.msg = fmt.Sprint(args...)
	runtime.Goexit()
}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// fatalOf runs fn with a fatalTB and returns its Fatal message ("" if none).
func fatalOf(fn func(tb testing.TB)) string {
	tb := &fatalTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(tb)
	}()
	<-done
	return tb.msg
}

func TestQueryTextNamesTheTablesUnqualified(t *testing.T) {
	got := queryText(Statement{QueryID: 7, Tables: []string{"public.orders", "app.items"}})
	want := "SELECT count(*) FROM orders, items WHERE id = $1 /* q7 */"
	if got != want {
		t.Fatalf("queryText = %q, want %q", got, want)
	}
}

func TestTablesDocListsEachTableOnce(t *testing.T) {
	doc := tablesDoc(t, []Statement{
		{QueryID: 1, Tables: []string{"public.orders", "app.items"}},
		{QueryID: 2, Tables: []string{"app.items"}},
	})
	var got []map[string]string
	if err := json.Unmarshal(doc, &got); err != nil {
		t.Fatalf("tables doc %s: %v", doc, err)
	}
	if len(got) != 2 || got[0]["schemaname"] != "public" || got[0]["relname"] != "orders" ||
		got[1]["schemaname"] != "app" || got[1]["relname"] != "items" {
		t.Fatalf("tables doc = %v, want public.orders then app.items once", got)
	}
	// Empty: no statements name no tables.
	if empty := string(tablesDoc(t, nil)); empty != "null" {
		t.Fatalf("tables doc without statements = %s, want null", empty)
	}
}

// fixture is two statements over cycles of 15 minutes from first, q1 with
// 10 calls a cycle and q2 with 3, and a statistics reset at the break.
func fixture(first time.Time, cycles int, brk time.Time) Source {
	src := Source{First: first, Last: first.Add(time.Duration(cycles) * 15 * time.Minute),
		Statements: []Statement{{QueryID: 1, Tables: []string{"public.orders"}},
			{QueryID: 2, Tables: []string{"public.items"}}},
		Cycles: map[int64][]Cycle{}}
	if !brk.IsZero() {
		src.Breaks = []time.Time{brk}
	}
	for i := 1; i <= cycles; i++ {
		to := first.Add(time.Duration(i) * 15 * time.Minute)
		src.Cycles[1] = append(src.Cycles[1], Cycle{To: to, Calls: 10, Blocks: 4, Rows: 10,
			Ms: 2.5})
		src.Cycles[2] = append(src.Cycles[2], Cycle{To: to, Calls: 3, Blocks: 1, Rows: 3, Ms: 1})
	}
	return src
}

func TestQueriesDocCountsCumulatively(t *testing.T) {
	first := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	src := fixture(first, 2, time.Time{})
	sum := map[int64]*counters{}
	calls := func(doc []byte) map[float64]float64 {
		var rows []map[string]any
		if err := json.Unmarshal(doc, &rows); err != nil {
			t.Fatalf("queries doc %s: %v", doc, err)
		}
		out := map[float64]float64{}
		for _, r := range rows {
			out[r["queryid"].(float64)] = r["calls"].(float64)
		}
		return out
	}
	// Zero: the first collection has no cycle behind it.
	if got := calls(queriesDoc(t, src, sum, first, 0)); got[1] != 0 || got[2] != 0 {
		t.Fatalf("first collection calls = %v, want zeros", got)
	}
	queriesDoc(t, src, sum, first.Add(15*time.Minute), 1)
	got := calls(queriesDoc(t, src, sum, first.Add(30*time.Minute), 2))
	if got[1] != 20 || got[2] != 6 {
		t.Fatalf("calls after two cycles = %v, want q1 20 and q2 6", got)
	}
}

// Invalid input: a cycle that does not end at its collection is refused.
func TestQueriesDocRefusesAMisalignedCycle(t *testing.T) {
	first := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	src := fixture(first, 2, time.Time{})
	msg := fatalOf(func(tb testing.TB) {
		queriesDoc(tb, src, map[int64]*counters{}, first.Add(20*time.Minute), 1)
	})
	if !strings.Contains(msg, "cycle 1 of query 1 ends at") {
		t.Fatalf("misaligned cycle: fatal %q, want the cycle named", msg)
	}
}

// Nil/zero and boundary: no cycles or a zero cycle length is refused
// before the database is touched (db is nil here).
func TestPersistRefusesAFixtureWithoutCycles(t *testing.T) {
	first := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for name, run := range map[string]func(tb testing.TB){
		"zero source": func(tb testing.TB) { Persist(tb, nil, Source{}, 15*time.Minute, 1) },
		"zero cycle":  func(tb testing.TB) { Persist(tb, nil, fixture(first, 1, time.Time{}), 0, 1) },
	} {
		if msg := fatalOf(run); !strings.Contains(msg, "the fixture has no cycles") {
			t.Errorf("%s: fatal %q, want the fixture refused", name, msg)
		}
	}
}
