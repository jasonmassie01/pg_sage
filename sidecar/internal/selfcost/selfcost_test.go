package selfcost

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

var keyA = StatementKey{UserID: 10, QueryID: 1, TopLevel: true}

// reading is one sample whose pg_sage statements are a single entry.
func reading(at time.Duration, dbMs float64, calls, blocks, read, written int64) Reading {
	return Reading{At: t0.Add(at), Database: "app", StatementsKnown: true,
		DBTimeMs: dbMs, Calls: calls, Blocks: blocks, RowsRead: read,
		RowsWritten: written, SchemaBytes: 4096,
		Statements: map[StatementKey]StatementCounters{
			keyA: {TimeMs: dbMs, Calls: calls, Blocks: blocks}}}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestBetween_ScalesWindowToCollectorCycle(t *testing.T) {
	prev := reading(0, 1000, 10, 100, 1000, 50)
	cur := reading(120*time.Second, 7000, 70, 700, 3000, 250)
	c := Between(prev, cur, 60*time.Second)
	if !c.Known || !c.DBTimeKnown {
		t.Fatalf("cost unknown: %+v", c)
	}
	// Two cycles in the window: per cycle is half the delta.
	checks := map[string][2]float64{
		"db ms":   {c.DBTimeMsPerCycle, 3000},
		"calls":   {c.CallsPerCycle, 30},
		"blocks":  {c.BlocksPerCycle, 300},
		"read":    {c.RowsReadPerCycle, 1000},
		"written": {c.RowsWrittenPerCycle, 100},
		"window":  {c.WindowSeconds, 120},
		"cycle":   {c.CycleSeconds, 60},
	}
	for name, v := range checks {
		if !near(v[0], v[1]) {
			t.Errorf("%s = %v, want %v", name, v[0], v[1])
		}
	}
	if c.SchemaBytes != 4096 || c.Database != "app" || !c.At.Equal(cur.At) {
		t.Errorf("cost identity = %+v", c)
	}
}

func TestBetween_NoPreviousReadingIsUnknownButKeepsSize(t *testing.T) {
	c := Between(Reading{}, reading(0, 5, 1, 1, 1, 1), time.Minute)
	if c.Known || c.DBTimeKnown {
		t.Fatalf("first reading produced a rate: %+v", c)
	}
	if c.SchemaBytes != 4096 {
		t.Fatalf("schema size lost on the first reading: %d", c.SchemaBytes)
	}
}

func TestBetween_InvalidWindowOrCycleIsUnknown(t *testing.T) {
	prev := reading(0, 1, 1, 1, 1, 1)
	for name, tc := range map[string]struct {
		cur   Reading
		cycle time.Duration
	}{
		"same instant":    {reading(0, 9, 9, 9, 9, 9), time.Minute},
		"clock went back": {reading(-time.Second, 9, 9, 9, 9, 9), time.Minute},
		"zero cycle":      {reading(time.Minute, 9, 9, 9, 9, 9), 0},
		"negative cycle":  {reading(time.Minute, 9, 9, 9, 9, 9), -time.Second},
	} {
		if c := Between(prev, tc.cur, tc.cycle); c.Known || c.DBTimeKnown {
			t.Errorf("%s: cost = %+v, want unknown", name, c)
		}
	}
}

// A statistics reset makes counters go backwards: that window has no rate.
func TestBetween_CounterResetIsUnknown(t *testing.T) {
	prev := reading(0, 1000, 100, 100, 1000, 1000)
	if c := Between(prev, reading(time.Minute, 2000, 200, 200, 10, 2000),
		time.Minute); c.Known {
		t.Errorf("rows-read reset: %+v, want unknown", c)
	}
	if c := Between(prev, reading(time.Minute, 2000, 200, 200, 2000, 10),
		time.Minute); c.Known {
		t.Errorf("rows-written reset: %+v, want unknown", c)
	}
	// An entry whose counters went back was reset: its whole count is new.
	c := Between(prev, reading(time.Minute, 10, 5, 200, 2000, 2000), time.Minute)
	if !c.Known || !c.DBTimeKnown || !near(c.DBTimeMsPerCycle, 10) || !near(c.CallsPerCycle, 5) {
		t.Errorf("pg_stat_statements entry reset: %+v, want 10 ms and 5 calls", c)
	}
}

func TestBetween_StatementsUnavailableLeavesDBTimeUnknown(t *testing.T) {
	prev := reading(0, 0, 0, 0, 0, 0)
	cur := reading(time.Minute, 0, 0, 0, 60, 6)
	prev.StatementsKnown, cur.StatementsKnown = false, false
	c := Between(prev, cur, time.Minute)
	if !c.Known || c.DBTimeKnown || !near(c.RowsReadPerCycle, 60) {
		t.Fatalf("cost = %+v, want table rates only", c)
	}
	cur.StatementsKnown = true // extension installed in between
	if c := Between(prev, cur, time.Minute); c.DBTimeKnown {
		t.Fatalf("DB time known across an install: %+v", c)
	}
}

func TestOverBudget_Boundaries(t *testing.T) {
	known := Cost{Known: true, DBTimeKnown: true, DBTimeMsPerCycle: 3000}
	cases := []struct {
		name   string
		cost   Cost
		budget int
		want   bool
	}{
		{"exactly at budget", known, 3000, false},
		{"one ms over", Cost{Known: true, DBTimeKnown: true, DBTimeMsPerCycle: 3000.5}, 3000,
			true},
		{"well under", known, 10000, false},
		{"disabled", known, 0, false},
		{"negative budget", known, -1, false},
		{"unknown DB time", Cost{Known: true, DBTimeMsPerCycle: 1e9}, 1, false},
		{"unknown window", Cost{DBTimeKnown: true, DBTimeMsPerCycle: 1e9}, 1, false},
	}
	for _, tc := range cases {
		if got := OverBudget(tc.cost, tc.budget); got != tc.want {
			t.Errorf("%s: OverBudget = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestMeter_ObserveKeepsPreviousAndLast(t *testing.T) {
	m := NewMeter()
	if m.Last().Known || m.Last().SchemaBytes != 0 {
		t.Fatalf("fresh meter has a cost: %+v", m.Last())
	}
	first := m.Observe(reading(0, 100, 1, 1, 1, 1), time.Minute)
	if first.Known || m.Last().SchemaBytes != 4096 {
		t.Fatalf("first observation = %+v", first)
	}
	second := m.Observe(reading(time.Minute, 400, 4, 4, 4, 4), time.Minute)
	if !second.Known || !near(second.DBTimeMsPerCycle, 300) {
		t.Fatalf("second observation = %+v, want 300 ms per cycle", second)
	}
	if m.Last() != second {
		t.Fatalf("Last = %+v, want %+v", m.Last(), second)
	}
	third := m.Observe(reading(2*time.Minute, 500, 5, 5, 5, 5), time.Minute)
	if !near(third.DBTimeMsPerCycle, 100) {
		t.Fatalf("third observation used the wrong baseline: %+v", third)
	}
}

func TestMeter_ConcurrentObserveAndLast(t *testing.T) {
	m := NewMeter()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			m.Observe(reading(time.Duration(i)*time.Second, float64(i), 1, 1, 1, 1),
				time.Minute)
		}(i)
		go func() {
			defer wg.Done()
			_ = m.Last()
		}()
	}
	wg.Wait()
	if m.Last().SchemaBytes != 4096 {
		t.Fatalf("last cost lost after concurrent use: %+v", m.Last())
	}
}

func TestNilMeterIsSafe(t *testing.T) {
	var m *Meter
	if c := m.Observe(reading(0, 1, 1, 1, 1, 1), time.Minute); c.Known {
		t.Fatalf("nil meter observed %+v", c)
	}
	if c := m.Last(); c.Known {
		t.Fatalf("nil meter last %+v", c)
	}
}

func TestStatementsUnavailableClassification(t *testing.T) {
	for code, want := range map[string]bool{
		"42P01": true,  // view missing: extension not installed
		"55000": true,  // not in shared_preload_libraries
		"42883": true,  // function missing
		"42501": false, // permission denied: a real error, surfaced
		"57014": false, // statement timeout: surfaced
	} {
		err := &pgconn.PgError{Code: code}
		if got := statementsUnavailable(err); got != want {
			t.Errorf("code %s: unavailable = %v, want %v", code, got, want)
		}
	}
	if statementsUnavailable(errors.New("conn reset")) {
		t.Error("a network error was classified as a missing extension")
	}
	if statementsUnavailable(nil) {
		t.Error("nil error classified as a missing extension")
	}
}

// pg_stat_statements evicts entries on a busy server: an evicted pg_sage
// entry must neither hide the window's cost nor make it negative, and a
// new entry counts in full (it was created inside the window).
func TestBetween_EvictedAndNewStatements(t *testing.T) {
	keyB := StatementKey{UserID: 10, QueryID: 2, TopLevel: true}
	keyC := StatementKey{UserID: 10, QueryID: 3, TopLevel: true}
	prev := reading(0, 0, 0, 0, 0, 0)
	prev.Statements = map[StatementKey]StatementCounters{
		keyA: {TimeMs: 100, Calls: 10, Blocks: 50}, keyB: {TimeMs: 500, Calls: 5, Blocks: 9}}
	cur := reading(time.Minute, 0, 0, 0, 0, 0)
	cur.Statements = map[StatementKey]StatementCounters{
		keyA: {TimeMs: 160, Calls: 16, Blocks: 80}, keyC: {TimeMs: 30, Calls: 3, Blocks: 4}}
	c := Between(prev, cur, time.Minute)
	if !c.DBTimeKnown || !near(c.DBTimeMsPerCycle, 90) || !near(c.CallsPerCycle, 9) ||
		!near(c.BlocksPerCycle, 34) {
		t.Fatalf("cost = %+v, want 90 ms, 9 calls, 34 blocks", c)
	}
}
