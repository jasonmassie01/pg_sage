package selfcost

import (
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// footprint-08: a statement's tag names the component that sent it
// (/* pg_sage sre:probe v1 */ is sre:probe), so pg_sage can say which part
// of it costs the database what.

func TestComponent_FromTag(t *testing.T) {
	cases := map[string]string{
		"SELECT /* pg_sage sre:probe v1 */ 1":            "sre:probe",
		"SELECT /* pg_sage collector */ count(*) FROM t": "collector",
		"/* pg_sage analyzer */ SELECT 1":                "analyzer",
		"WITH /* pg_sage tuner:stale_stats */ x AS (..)": "tuner:stale_stats",
		"SELECT /*pg_sage   executor*/ 1":                "executor",
		"SELECT /* pg_sage\n\tfacts */ 1":                "facts",
		"SELECT /* PG_SAGE Collector */ 1":               "collector",
		"SELECT /* Pg_Sage SRE:Probe V1 */ 1":            "sre:probe",
		"SELECT /* pg_sage */ 1":                         Unattributed,
		"SELECT /* PG_SAGE */ 1":                         Unattributed,
		"SELECT /*pg_sage*/ 1":                           Unattributed,
		"SELECT 1":                                       Unattributed,
		"SELECT /* other tool */ 1":                      Unattributed,
		"":                                               Unattributed,
	}
	for in, want := range cases {
		if got := Component(in); got != want {
			t.Errorf("Component(%q) = %q, want %q", in, got, want)
		}
	}
}

// Invalid input: only [a-z0-9_:.-]{1,32} after the tag is a component; a
// statement cannot forge one with a quote, a nested comment or text after
// the tag's end.
func TestComponent_CannotBeForged(t *testing.T) {
	cases := map[string]string{
		"SELECT /* pg_sage sre'; DROP TABLE t; -- */ 1":          Unattributed,
		"SELECT /* pg_sage /*executor */ 1":                      Unattributed,
		"SELECT /* pg_sage */ 1 /* pg_sage executor */":          Unattributed,
		"SELECT /* pg_sage collector*/ executor */ 1":            "collector",
		"SELECT /* pg_sage sre:probe*/*/ 1":                      "sre:probe",
		"SELECT /* pg_sage col$lector */ 1":                      Unattributed,
		"SELECT /* pg_sage collector":                            Unattributed,
		"SELECT /* pg_sage éxecutor */ 1":                        Unattributed,
		"SELECT /* pg_sage " + strings.Repeat("a", 33) + " */ 1": Unattributed,
		"SELECT /* pg_sagex executor */ 1":                       Unattributed,
	}
	for in, want := range cases {
		if got := Component(in); got != want {
			t.Errorf("Component(%q) = %q, want %q", in, got, want)
		}
	}
}

// Boundary: a 32-character component is the longest allowed.
func TestComponent_LengthBoundary(t *testing.T) {
	max := strings.Repeat("a", 32)
	if got := Component("SELECT /* pg_sage " + max + " */ 1"); got != max {
		t.Fatalf("32 characters = %q, want %q", got, max)
	}
	if got := Component("SELECT /* pg_sage a */ 1"); got != "a" {
		t.Fatalf("1 character = %q, want a", got)
	}
}

// The metric folds components into a fixed set by their family (the part
// before ':'), so its label cardinality stays bounded.
func TestMetricComponent_FixedSet(t *testing.T) {
	cases := map[string]string{
		"collector": "collector", "analyzer": "analyzer", "executor": "executor",
		"facts": "facts", "tuner": "tuner", "optimizer": "optimizer",
		"sre:probe": "sre", "sre": "sre", "tuner:stale_stats": "tuner",
		Unattributed: Unattributed, "audit_chain": "other", "text": "other",
		"": "other", "collectorx": "other", ":collector": "other",
	}
	for in, want := range cases {
		if got := MetricComponent(in); got != want {
			t.Errorf("MetricComponent(%q) = %q, want %q", in, got, want)
		}
	}
	seen := map[string]bool{}
	for _, c := range MetricComponents {
		if seen[c] {
			t.Fatalf("MetricComponents repeats %q", c)
		}
		seen[c] = true
		if MetricComponent(c) != c {
			t.Errorf("MetricComponent(%q) = %q, want itself", c, MetricComponent(c))
		}
	}
	if !seen["other"] || !seen[Unattributed] {
		t.Fatalf("MetricComponents %v must hold other and %s", MetricComponents, Unattributed)
	}
}

func componentReadings(t0 time.Time) (Reading, Reading) {
	prev := Reading{At: t0, StatementsKnown: true, Statements: map[StatementKey]StatementCounters{
		key(1): {TimeMs: 100, Calls: 10, Blocks: 50, Text: "SELECT /* pg_sage collector */ 1"},
		key(2): {TimeMs: 500, Calls: 5, Blocks: 10, Text: "SELECT /* pg_sage sre:probe v1 */ 2"},
		key(3): {TimeMs: 40, Calls: 4, Blocks: 4, Text: "SELECT /* pg_sage */ 3"},
		key(5): {TimeMs: 9, Calls: 9, Blocks: 9, Text: "SELECT /* pg_sage analyzer */ 5"},
	}}
	cur := Reading{At: t0.Add(10 * time.Minute), StatementsKnown: true,
		Statements: map[StatementKey]StatementCounters{
			key(1): {TimeMs: 400, Calls: 20, Blocks: 80, Text: "SELECT /* pg_sage collector */ 1"},
			key(2): {TimeMs: 600, Calls: 7, Blocks: 30,
				Text: "SELECT /* pg_sage sre:probe v1 */ 2"},
			key(3): {TimeMs: 10, Calls: 1, Blocks: 1, Text: "SELECT /* pg_sage */ 3"}, // reset
			key(4): {TimeMs: 200, Calls: 2, Blocks: 6, Text: "SELECT /* pg_sage collector */ 4"},
			key(5): {TimeMs: 9, Calls: 9, Blocks: 9, Text: "SELECT /* pg_sage analyzer */ 5"},
		}}
	return prev, cur
}

func TestMeter_ByComponentPerHour(t *testing.T) {
	m := NewMeter()
	prev, cur := componentReadings(time.Now())
	m.Observe(prev, time.Minute)
	if got := m.ByComponent(); got != nil {
		t.Fatalf("by component before a window = %+v, want nil", got)
	}
	cost := m.Observe(cur, time.Minute)
	got := m.ByComponent()
	// 10-minute window: x6 per hour. collector = (300+200) ms, 12 calls,
	// 36 blocks; sre:probe 100 ms, 2 calls, 20 blocks; the reset bare entry
	// counts from zero: 10 ms, 1 call, 1 block; analyzer did no work.
	want := []ComponentCost{
		{Component: "collector", DBTimeMsPerHour: 3000, CallsPerHour: 72, BlocksPerHour: 216},
		{Component: "sre:probe", DBTimeMsPerHour: 600, CallsPerHour: 12, BlocksPerHour: 120},
		{Component: Unattributed, DBTimeMsPerHour: 60, CallsPerHour: 6, BlocksPerHour: 6},
	}
	if len(got) != len(want) {
		t.Fatalf("by component = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Component != want[i].Component ||
			!nearRel(got[i].DBTimeMsPerHour, want[i].DBTimeMsPerHour) ||
			!nearRel(got[i].CallsPerHour, want[i].CallsPerHour) ||
			!nearRel(got[i].BlocksPerHour, want[i].BlocksPerHour) {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	assertSumsEqualTotal(t, got, cost)
}

func assertSumsEqualTotal(t *testing.T, rows []ComponentCost, cost Cost) {
	t.Helper()
	var dbTime, calls, blocks float64
	for _, r := range rows {
		dbTime += r.DBTimeMsPerHour
		calls += r.CallsPerHour
		blocks += r.BlocksPerHour
	}
	perHour := 3600 / cost.CycleSeconds
	if !nearRel(dbTime, cost.DBTimeMsPerCycle*perHour) ||
		!nearRel(calls, cost.CallsPerCycle*perHour) ||
		!nearRel(blocks, cost.BlocksPerCycle*perHour) {
		t.Fatalf("sums %.3f ms, %.3f calls, %.3f blocks per hour; total %+v", dbTime, calls,
			blocks, cost)
	}
}

func nearRel(a, b float64) bool { return math.Abs(a-b) < 1e-6*math.Max(1, math.Abs(b)) }

// Nil/empty: no statements, no window or a meter that was never built
// give no rows, never zero rows that look measured.
func TestMeter_ByComponentUnknown(t *testing.T) {
	t0 := time.Now()
	m := NewMeter()
	prev, _ := componentReadings(t0)
	m.Observe(prev, time.Minute)
	m.Observe(Reading{At: t0.Add(time.Minute)}, time.Minute) // pg_stat_statements gone
	if got := m.ByComponent(); got != nil {
		t.Fatalf("without statements = %+v, want nil", got)
	}
	var nilMeter *Meter
	if got := nilMeter.ByComponent(); got != nil {
		t.Fatalf("nil meter = %+v", got)
	}
	if got := ByComponent(nil, nil, 600); len(got) != 0 {
		t.Fatalf("no entries = %+v", got)
	}
	_, cur := componentReadings(t0)
	if got := ByComponent(nil, cur.Statements, 0); got != nil {
		t.Fatalf("zero window = %+v, want nil", got)
	}
	if got := ByComponent(nil, cur.Statements, -5); got != nil {
		t.Fatalf("negative window = %+v, want nil", got)
	}
}

func TestMeter_ByComponentReturnsCopy(t *testing.T) {
	m := NewMeter()
	prev, cur := componentReadings(time.Now())
	m.Observe(prev, time.Minute)
	m.Observe(cur, time.Minute)
	got := m.ByComponent()
	got[0].Component = "mutated"
	if m.ByComponent()[0].Component != "collector" {
		t.Fatal("ByComponent must return a copy")
	}
}

// Ties order by component name, so the API and metrics are stable.
func TestByComponent_TiesOrderByName(t *testing.T) {
	cur := map[StatementKey]StatementCounters{
		key(1): {TimeMs: 10, Calls: 1, Text: "SELECT /* pg_sage tuner */ 1"},
		key(2): {TimeMs: 10, Calls: 1, Text: "SELECT /* pg_sage facts */ 2"},
	}
	for i := 0; i < 20; i++ {
		got := ByComponent(nil, cur, 3600)
		if len(got) != 2 || got[0].Component != "facts" || got[1].Component != "tuner" {
			t.Fatalf("tie order = %+v, want facts then tuner", got)
		}
	}
}

func TestTopStatements_CarryComponent(t *testing.T) {
	prev, cur := componentReadings(time.Now())
	got := TopStatements(prev.Statements, cur.Statements, 5)
	byText := map[string]string{}
	for _, s := range got {
		byText[s.Text] = s.Component
	}
	for text, want := range map[string]string{
		"SELECT 1": "collector", "SELECT 2": "sre:probe", "SELECT 3": Unattributed,
		"SELECT 4": "collector",
	} {
		if byText[text] != want {
			t.Errorf("component of %q = %q, want %q (top %+v)", text, byText[text], want, got)
		}
	}
}

// Concurrent access: the analyzer observes while the API reads.
func TestMeter_ConcurrentByComponent(t *testing.T) {
	m := NewMeter()
	t0 := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r := Reading{At: t0.Add(time.Duration(i*1000+j) * time.Second),
					StatementsKnown: true, Statements: map[StatementKey]StatementCounters{
						key(1): {TimeMs: float64(j), Calls: int64(j),
							Text: "SELECT /* pg_sage collector */ 1"}}}
				m.Observe(r, time.Minute)
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				for _, c := range m.ByComponent() {
					if c.Component != "collector" {
						t.Errorf("component %q, want collector", c.Component)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
