package selfcost

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func key(id int64) StatementKey { return StatementKey{UserID: 10, QueryID: id, TopLevel: true} }

func TestTopStatements_DeltaRankedByTime(t *testing.T) {
	prev := map[StatementKey]StatementCounters{
		key(1): {TimeMs: 100, Calls: 10, Blocks: 50, Text: "SELECT /* pg_sage */ 1"},
		key(2): {TimeMs: 500, Calls: 5, Blocks: 10, Text: "SELECT /* pg_sage */ 2"},
	}
	cur := map[StatementKey]StatementCounters{
		key(1): {TimeMs: 400, Calls: 20, Blocks: 80, Text: "SELECT /* pg_sage */ 1"},
		key(2): {TimeMs: 550, Calls: 6, Blocks: 12, Text: "SELECT /* pg_sage */ 2"},
		key(3): {TimeMs: 120, Calls: 2, Blocks: 900, Text: "SELECT /* pg_sage */ 3"},
	}
	got := TopStatements(prev, cur, 2)
	if len(got) != 2 {
		t.Fatalf("top = %+v, want 2", got)
	}
	if got[0].Text != "SELECT 1" || got[0].TimeMs != 300 || got[0].Calls != 10 ||
		got[0].Blocks != 30 {
		t.Fatalf("first = %+v, want statement 1 with its delta", got[0])
	}
	if got[1].Text != "SELECT 3" || got[1].TimeMs != 120 || got[1].Blocks != 900 {
		t.Fatalf("second = %+v, want new statement 3 counted in full", got[1])
	}
}

// A reset entry (counters went back) counts from zero, like statementDelta.
func TestTopStatements_ResetEntryCountsFromZero(t *testing.T) {
	prev := map[StatementKey]StatementCounters{key(1): {TimeMs: 900, Calls: 90, Text: "x"}}
	cur := map[StatementKey]StatementCounters{key(1): {TimeMs: 40, Calls: 4, Text: "x"}}
	got := TopStatements(prev, cur, 5)
	if len(got) != 1 || got[0].TimeMs != 40 || got[0].Calls != 4 {
		t.Fatalf("top = %+v, want the reset entry from zero", got)
	}
}

func TestTopStatements_IdleAndEmpty(t *testing.T) {
	same := map[StatementKey]StatementCounters{key(1): {TimeMs: 5, Calls: 1, Text: "x"}}
	if got := TopStatements(same, same, 5); len(got) != 0 {
		t.Fatalf("no work: %+v, want none", got)
	}
	if got := TopStatements(nil, nil, 5); len(got) != 0 {
		t.Fatalf("nil: %+v", got)
	}
	if got := TopStatements(nil, same, 0); len(got) != 0 {
		t.Fatalf("n=0: %+v", got)
	}
}

func TestTopStatements_TiesAreStable(t *testing.T) {
	cur := map[StatementKey]StatementCounters{
		key(2): {TimeMs: 10, Calls: 1, Text: "b"},
		key(1): {TimeMs: 10, Calls: 1, Text: "a"},
	}
	for i := 0; i < 20; i++ {
		got := TopStatements(nil, cur, 2)
		if got[0].Text != "a" || got[1].Text != "b" {
			t.Fatalf("tie order = %+v, want by text", got)
		}
	}
}

// Statement text is shown in a finding: the pg_sage tag is removed (the
// self-monitoring filter drops findings mentioning pg_sage), whitespace is
// collapsed and long text is cut.
func TestStatementText(t *testing.T) {
	cases := map[string]string{
		"SELECT /* pg_sage */ 1":                      "SELECT 1",
		"/* pg_sage */ SELECT\n\t a,\n b FROM t":       "SELECT a, b FROM t",
		"WITH /* pg_sage sre:runway v2 */ x AS (...)": "WITH x AS (...)",
		"SELECT /* other */ 1":                        "SELECT /* other */ 1",
		"":                                            "",
	}
	for in, want := range cases {
		if got := StatementText(in); got != want {
			t.Errorf("StatementText(%q) = %q, want %q", in, got, want)
		}
	}
	long := StatementText("SELECT " + strings.Repeat("a, ", 200) + "b")
	if len(long) > MaxStatementText || !strings.HasSuffix(long, "...") {
		t.Fatalf("long text = %d chars %q, want cut to %d with ...", len(long),
			long[len(long)-10:], MaxStatementText)
	}
	if strings.Contains(strings.ToLower(StatementText("/* PG_SAGE */ SELECT 1")), "pg_sage") {
		t.Fatal("upper-case tag kept")
	}
}

func TestMeter_TopFollowsObservations(t *testing.T) {
	m := NewMeter()
	t0 := time.Now()
	first := Reading{At: t0, StatementsKnown: true,
		Statements: map[StatementKey]StatementCounters{key(1): {TimeMs: 10, Calls: 1,
			Text: "SELECT /* pg_sage */ 1"}}}
	m.Observe(first, time.Minute)
	if got := m.Top(); len(got) != 0 {
		t.Fatalf("top after the first reading = %+v, want none (no window)", got)
	}
	second := Reading{At: t0.Add(time.Minute), StatementsKnown: true,
		Statements: map[StatementKey]StatementCounters{key(1): {TimeMs: 70, Calls: 4,
			Text: "SELECT /* pg_sage */ 1"}}}
	m.Observe(second, time.Minute)
	got := m.Top()
	if len(got) != 1 || got[0].TimeMs != 60 || got[0].Text != "SELECT 1" {
		t.Fatalf("top = %+v, want SELECT 1 with 60 ms", got)
	}
	got[0].Text = "mutated"
	if m.Top()[0].Text != "SELECT 1" {
		t.Fatal("Top must return a copy")
	}
	var nilMeter *Meter
	if nilMeter.Top() != nil {
		t.Fatal("nil meter Top must be nil")
	}
}

func TestMeter_TopUnknownWithoutStatements(t *testing.T) {
	m := NewMeter()
	t0 := time.Now()
	m.Observe(Reading{At: t0, StatementsKnown: true, Statements: map[StatementKey]StatementCounters{
		key(1): {TimeMs: 1, Calls: 1, Text: "x"}}}, time.Minute)
	m.Observe(Reading{At: t0.Add(time.Minute)}, time.Minute)
	if got := m.Top(); len(got) != 0 {
		t.Fatalf("top without pg_stat_statements = %+v, want none", got)
	}
}

func TestMeter_ConcurrentTop(t *testing.T) {
	m := NewMeter()
	var wg sync.WaitGroup
	t0 := time.Now()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r := Reading{At: t0.Add(time.Duration(i*1000+j) * time.Second),
					StatementsKnown: true, Statements: map[StatementKey]StatementCounters{
						key(1): {TimeMs: float64(j), Calls: int64(j), Text: "x"}}}
				m.Observe(r, time.Minute)
				_ = m.Top()
			}
		}(i)
	}
	wg.Wait()
}
