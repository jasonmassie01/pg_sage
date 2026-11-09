package analyzer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/config"
)

// rules-06 unit tests: the publish-time suppression filter, the reopen
// window and the evidence-time decision after an action resolution.

// countingQuerier records Query calls and fails each one with err.
type countingQuerier struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (q *countingQuerier) Query(
	_ context.Context, _ string, _ ...any,
) (pgx.Rows, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls++
	return nil, q.err
}

func (q *countingQuerier) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.calls
}

func TestWithoutActiveSuppressions_EmptyIssuesNoQuery(t *testing.T) {
	q := &countingQuerier{err: errors.New("must not be called")}
	for _, in := range [][]Finding{nil, {}} {
		out, err := withoutActiveSuppressions(context.Background(), q, in)
		if err != nil {
			t.Fatalf("empty input: err = %v, want nil", err)
		}
		if len(out) != 0 {
			t.Fatalf("empty input: out = %+v, want empty", out)
		}
	}
	if q.count() != 0 {
		t.Fatalf("anti-join queries for empty input = %d, want 0", q.count())
	}
}

func TestWithoutActiveSuppressions_QueryErrorPropagates(t *testing.T) {
	q := &countingQuerier{err: errors.New("connection reset by peer")}
	in := []Finding{lifecycleFinding("cat_a", "public.a", "warning")}
	out, err := withoutActiveSuppressions(context.Background(), q, in)
	if err == nil {
		t.Fatal("err = nil, want the query error")
	}
	if !strings.Contains(err.Error(), "connection reset by peer") ||
		!strings.Contains(err.Error(), "suppress") {
		t.Fatalf("err = %q, want context naming suppression and the cause", err)
	}
	if out != nil {
		t.Fatalf("out = %+v, want nil on error (fail closed)", out)
	}
	if q.count() != 1 {
		t.Fatalf("queries = %d, want exactly 1 set-based query", q.count())
	}
}

// No pool: an empty cycle must not touch the database at all, and the
// published set becomes empty.
func TestFinalizeCycle_EmptyCycleIssuesNoAntiJoin(t *testing.T) {
	a := New(nil, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	q := &countingQuerier{err: errors.New("must not be called")}
	a.suppressions = q
	a.findings = []Finding{lifecycleFinding("cat_prev", "public.p", "info")}
	a.finalizeCycle(context.Background(), nil, map[string]bool{})
	if q.count() != 0 {
		t.Fatalf("anti-join queries = %d, want 0", q.count())
	}
	if got := a.LatestFindings(); len(got) != 0 {
		t.Fatalf("LatestFindings = %+v, want empty after an empty cycle", got)
	}
}

func TestReopenWindow(t *testing.T) {
	def := 2 * config.DefaultAnalyzerInterval
	cases := []struct {
		name string
		cfg  *config.Config
		want time.Duration
	}{
		{"nil config uses defaults", nil, def},
		{"zero intervals use defaults", &config.Config{}, def},
		{"analyzer dominates", cfgIntervals(600, 60), 1200 * time.Second},
		{"collector dominates", cfgIntervals(30, 90), 180 * time.Second},
		{"equal intervals", cfgIntervals(60, 60), 120 * time.Second},
		{"negative falls back", cfgIntervals(-5, -5), def},
	}
	for _, tc := range cases {
		if got := reopenWindow(tc.cfg); got != tc.want {
			t.Errorf("%s: reopenWindow = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func cfgIntervals(analyzerSec, collectorSec int) *config.Config {
	return &config.Config{
		Analyzer:  config.AnalyzerConfig{IntervalSeconds: analyzerSec},
		Collector: config.CollectorConfig{IntervalSeconds: collectorSec},
	}
}

func TestDecideAfterAction(t *testing.T) {
	resolved := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	window := 1200 * time.Second
	cases := []struct {
		name      string
		collected time.Time
		dbNow     time.Time
		want      afterAction
	}{
		{"evidence before resolution is stale",
			resolved.Add(-time.Second), resolved.Add(time.Hour), afterActionStale},
		{"evidence at resolution instant is stale",
			resolved, resolved.Add(time.Hour), afterActionStale},
		{"evidence just after resolution reopens",
			resolved.Add(time.Second), resolved.Add(time.Second), afterActionReopen},
		{"evidence at T+600s reopens",
			resolved.Add(600 * time.Second), resolved.Add(601 * time.Second),
			afterActionReopen},
		{"evidence exactly at window edge reopens",
			resolved.Add(window), resolved.Add(window), afterActionReopen},
		{"evidence past window opens a new row",
			resolved.Add(window + time.Second), resolved.Add(window + time.Second),
			afterActionInsert},
		{"unknown evidence within its assumed age is stale",
			time.Time{}, resolved.Add(unknownEvidenceAge), afterActionStale},
		{"unknown evidence just past its assumed age reopens",
			time.Time{}, resolved.Add(unknownEvidenceAge + time.Second),
			afterActionReopen},
		{"unknown evidence 10 minutes after reopens",
			time.Time{}, resolved.Add(10 * time.Minute), afterActionReopen},
		{"unknown evidence past window opens a new row",
			time.Time{}, resolved.Add(window + unknownEvidenceAge + time.Second),
			afterActionInsert},
	}
	for _, tc := range cases {
		got := decideAfterAction(resolved, tc.collected, tc.dbNow, window)
		if got != tc.want {
			t.Errorf("%s: decideAfterAction = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestDetailWithPersistedAfterAction_CopiesDetail(t *testing.T) {
	f := lifecycleFinding("cat_a", "public.a", "warning")
	f.ActionRisk = "safe"
	raw, err := detailWithPersistedAfterAction(f, 42)
	if err != nil {
		t.Fatalf("detailWithPersistedAfterAction: %v", err)
	}
	got := string(raw)
	for _, want := range []string{`"persisted_after_action":42`, `"k":"v"`,
		`"action_risk":"safe"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("detail %s lacks %s", got, want)
		}
	}
	if _, ok := f.Detail[DetailPersistedAfterAction]; ok {
		t.Fatal("caller's Detail map was written")
	}
	f.Detail = nil
	raw, err = detailWithPersistedAfterAction(f, 7)
	if err != nil || !strings.Contains(string(raw), `"persisted_after_action":7`) {
		t.Fatalf("nil detail: raw=%s err=%v", raw, err)
	}
}
