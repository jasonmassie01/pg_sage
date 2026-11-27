package analyzer

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/selfcost"
)

// footprint-08: the analyzer hands the API one snapshot of what pg_sage
// costs this database, and the sage_self_cost advice names the component
// that cost most instead of asking the DBA to sort pg_stat_statements.

func footprintReadings(t0 time.Time) (selfcost.Reading, selfcost.Reading) {
	k1 := selfcost.StatementKey{UserID: 1, QueryID: 1, TopLevel: true}
	k2 := selfcost.StatementKey{UserID: 1, QueryID: 2, TopLevel: true}
	prev := selfcost.Reading{At: t0, Database: "app", StatementsKnown: true, SchemaBytes: 10,
		Statements: map[selfcost.StatementKey]selfcost.StatementCounters{
			k1: {TimeMs: 10, Calls: 1, Text: "SELECT /* pg_sage executor */ 1"},
			k2: {TimeMs: 10, Calls: 1, Text: "SELECT /* pg_sage */ 2"}}}
	cur := selfcost.Reading{At: t0.Add(time.Hour), Database: "app", StatementsKnown: true,
		SchemaBytes: 20, Statements: map[selfcost.StatementKey]selfcost.StatementCounters{
			k1: {TimeMs: 910, Calls: 11, Text: "SELECT /* pg_sage executor */ 1"},
			k2: {TimeMs: 110, Calls: 2, Text: "SELECT /* pg_sage */ 2"}}}
	return prev, cur
}

func TestSelfFootprint_SnapshotOfTheMeter(t *testing.T) {
	a := New(nil, testConfig(), nil, nil, nil, nil, nil, noopLog)
	m := selfcost.NewMeter()
	a.WithSelfCostMeter(m)
	if fp := a.SelfFootprint(); fp.Cost.Known || fp.ByComponent != nil || fp.Top != nil ||
		fp.Loops != nil {
		t.Fatalf("before any cycle = %+v, want empty", fp)
	}
	prev, cur := footprintReadings(time.Now())
	m.Observe(prev, time.Minute)
	m.Observe(cur, time.Minute)
	fp := a.SelfFootprint()
	if !fp.Cost.Known || !fp.Cost.DBTimeKnown || fp.Cost.SchemaBytes != 20 {
		t.Fatalf("cost = %+v, want the meter's window", fp.Cost)
	}
	if len(fp.ByComponent) != 2 || fp.ByComponent[0].Component != "executor" ||
		fp.ByComponent[0].DBTimeMsPerHour != 900 {
		t.Fatalf("by component = %+v, want executor 900 ms/h first", fp.ByComponent)
	}
	if len(fp.Top) != 2 || fp.Top[0].Component != "executor" {
		t.Fatalf("top = %+v", fp.Top)
	}
}

// Nil: an analyzer given a nil meter keeps its own.
func TestSelfFootprint_NilMeterKeepsDefault(t *testing.T) {
	a := New(nil, testConfig(), nil, nil, nil, nil, nil, noopLog)
	a.WithSelfCostMeter(nil)
	if fp := a.SelfFootprint(); fp.Cost.Known {
		t.Fatalf("fresh analyzer = %+v", fp)
	}
	var nilAnalyzer *Analyzer
	if fp := nilAnalyzer.SelfFootprint(); fp.Cost.Known || fp.ByComponent != nil {
		t.Fatalf("nil analyzer = %+v", fp)
	}
}

func TestRuleSelfCost_AdviceNamesTopComponent(t *testing.T) {
	comps := []selfcost.ComponentCost{
		{Component: "executor", DBTimeMsPerHour: 216000, CallsPerHour: 60},
		{Component: selfcost.Unattributed, DBTimeMsPerHour: 36000},
	}
	f := ruleSelfCost(knownCost(4200), 3000, comps...)[0]
	for _, want := range []string{"executor", "86%", "/api/v1/self-cost?database=app",
		"Settings > General > Footprint", "unattributed"} {
		if !strings.Contains(f.Recommendation, want) {
			t.Errorf("recommendation %q lacks %q", f.Recommendation, want)
		}
	}
	if strings.Contains(f.Recommendation, "sort them by total_exec_time") {
		t.Errorf("recommendation still asks to sort pg_stat_statements: %q", f.Recommendation)
	}
	if f.Detail["top_component"] != "executor" {
		t.Errorf("detail top_component = %v", f.Detail["top_component"])
	}
}

// Without a component breakdown (pg_stat_statements entries not yet
// attributed) the advice still points to the footprint, names nothing.
func TestRuleSelfCost_AdviceWithoutComponents(t *testing.T) {
	f := ruleSelfCost(knownCost(4200), 3000)[0]
	if !strings.Contains(f.Recommendation, "/api/v1/self-cost?database=app") {
		t.Errorf("recommendation %q does not point to the footprint", f.Recommendation)
	}
	if _, ok := f.Detail["top_component"]; ok {
		t.Errorf("detail names a top component without a breakdown: %v", f.Detail)
	}
	only := []selfcost.ComponentCost{{Component: selfcost.Unattributed, DBTimeMsPerHour: 5}}
	f = ruleSelfCost(knownCost(4200), 3000, only...)[0]
	if !strings.Contains(f.Recommendation, "unattributed") ||
		!strings.Contains(f.Recommendation, "100%") {
		t.Errorf("all-unattributed advice = %q", f.Recommendation)
	}
	zero := []selfcost.ComponentCost{{Component: "executor"}}
	f = ruleSelfCost(knownCost(4200), 3000, zero...)[0]
	if _, ok := f.Detail["top_component"]; ok {
		t.Errorf("zero-time breakdown named a top component: %v", f.Detail)
	}
}
