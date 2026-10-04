package analyzer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// The tuning agent (roadmap 2.2) is the analyzer's one tuning producer:
// its findings join the cycle, the categories it judged resolve, the ones
// it could not judge never resolve, and the tables it proposes indexes
// for are deferred by the tuner.

type fakeTuning struct {
	mu        sync.Mutex
	out       TuningOutput
	err       error
	cur, prev *collector.Snapshot
	calls     int
	stats     TuningStats
}

func (f *fakeTuning) Tune(_ context.Context, cur, prev *collector.Snapshot) (TuningOutput,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.cur, f.prev = cur, prev
	return f.out, f.err
}

func (f *fakeTuning) Stats() TuningStats { return f.stats }

type deferRecorder struct {
	deferred map[string]bool
	findings []Finding
}

func (d *deferRecorder) Tune(_ context.Context, deferred map[string]bool) ([]Finding, error) {
	d.deferred = deferred
	return d.findings, nil
}

func tuningAnalyzer(tp TuningProducer, qt QueryTuner, logs *[]string) *Analyzer {
	a := New(nil, &config.Config{}, nil, tp, nil, nil, qt,
		func(level, format string, args ...any) {
			*logs = append(*logs, level+" "+format)
		})
	a.eval = newCycleEval()
	return a
}

func indexedSnap() *collector.Snapshot {
	return &collector.Snapshot{CollectedAt: time.Now(),
		Indexes: []collector.IndexStats{{SchemaName: "public", RelName: "orders"}}}
}

func TestRunProducers_TuningFindingsEvaluationAndDeferral(t *testing.T) {
	tp := &fakeTuning{out: TuningOutput{
		Findings: []Finding{{Category: "missing_index",
			ObjectIdentifier: "public.orders|btree(customer_id)"}},
		Evaluated:   []string{"missing_index", "memory_tuning"},
		IndexTables: []string{"public.orders", `"Sales"."Orders"`},
	}}
	qt := &deferRecorder{findings: []Finding{{Category: "query_tuning",
		ObjectIdentifier: "queryid:1"}}}
	var logs []string
	a := tuningAnalyzer(tp, qt, &logs)
	cur, prev := indexedSnap(), indexedSnap()
	out := a.runProducers(context.Background(), cur, prev)
	if tp.calls != 1 || tp.cur != cur || tp.prev != prev {
		t.Fatalf("the agent sees both snapshots: calls %d", tp.calls)
	}
	if len(out) != 2 || out[0].Category != "missing_index" || out[1].Category != "query_tuning" {
		t.Fatalf("findings = %+v", out)
	}
	if !qt.deferred["public.orders"] || !qt.deferred[canonicalTable(`"Sales"."Orders"`)] {
		t.Fatalf("deferred = %v", qt.deferred)
	}
	got := a.eval.resolvable(nil)
	if !got["missing_index"] || !got["memory_tuning"] {
		t.Fatalf("resolvable = %v", got)
	}
}

func TestRunProducers_TuningFailureResolvesNothing(t *testing.T) {
	tp := &fakeTuning{err: errors.New("open findings unreadable"),
		out: TuningOutput{Failed: []string{"missing_index", "vacuum_tuning"}}}
	var logs []string
	a := tuningAnalyzer(tp, nil, &logs)
	out := a.runProducers(context.Background(), indexedSnap(), nil)
	if len(out) != 0 {
		t.Fatalf("findings = %+v", out)
	}
	got := a.eval.resolvable([]Finding{{Category: "missing_index"}})
	if got["missing_index"] || got["vacuum_tuning"] {
		t.Fatalf("a failed agent leaves its categories alone: %v", got)
	}
	if !slices.ContainsFunc(logs, func(l string) bool {
		return strings.HasPrefix(l, "WARN") && strings.Contains(l, "tuning")
	}) {
		t.Fatalf("logs = %v", logs)
	}
}

func TestRunProducers_FailedCategoriesWithoutAnError(t *testing.T) {
	tp := &fakeTuning{out: TuningOutput{Failed: []string{"missing_index"}}}
	var logs []string
	a := tuningAnalyzer(tp, nil, &logs)
	a.runProducers(context.Background(), indexedSnap(), nil)
	if a.eval.resolvable([]Finding{{Category: "missing_index"}})["missing_index"] {
		t.Fatal("a cold start leaves the agent's categories unresolved")
	}
}

func TestRunProducers_NoIndexListSkipsTheAgent(t *testing.T) {
	tp := &fakeTuning{out: TuningOutput{Evaluated: []string{"missing_index"}}}
	var logs []string
	a := tuningAnalyzer(tp, nil, &logs)
	snap := &collector.Snapshot{Unavailable: map[string]string{"indexes": "timeout"}}
	a.runProducers(context.Background(), snap, nil)
	if tp.calls != 0 {
		t.Fatal("without the index list the agent would see tables as unindexed")
	}
	if a.eval.resolvable([]Finding{{Category: "missing_index"}})["missing_index"] {
		t.Fatal("open index proposals must not resolve on a cycle the agent skipped")
	}
}

func TestTuningStats(t *testing.T) {
	var nilAnalyzer *Analyzer
	if _, ok := nilAnalyzer.TuningStats(); ok {
		t.Fatal("a nil analyzer has no tuning agent")
	}
	if _, ok := (&Analyzer{}).TuningStats(); ok {
		t.Fatal("no agent, no stats")
	}
	tp := &fakeTuning{stats: TuningStats{WhatIfSkipped: 3, ModelCallsSkipped: 2}}
	stats, ok := (&Analyzer{tuning: tp}).TuningStats()
	if !ok || stats != tp.stats {
		t.Fatalf("stats = %+v ok %v", stats, ok)
	}
}

func TestOptimizerRecommendationFindingIsExported(t *testing.T) {
	// The agent maps admitted index proposals with the analyzer's own
	// mapping, so the executor's what-if gate and identity stay one rule.
	rec := optimizer.Recommendation{Table: "public.orders", Severity: "info",
		DDL:    "CREATE INDEX CONCURRENTLY orders_a_idx ON public.orders (a)",
		WhatIf: optimizer.WhatIfVerified, Validated: true}
	f := OptimizerRecommendationFinding(rec, "explain_cache")
	if f.Detail["what_if_verdict"] == nil || f.Detail["plan_source"] != "explain_cache" {
		t.Fatalf("finding = %+v", f)
	}
}
