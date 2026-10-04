package optimizer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// memStore is an in-memory rejectionStore with the upsert semantics of
// sage.optimizer_rejection: one row per table and shape hash, a repeated
// measurement replaces the evidence and bumps the count.
type memStore struct {
	mu        sync.Mutex
	rows      map[string]rejection
	loadErr   error
	recordErr error
	loads     int
	records   int
}

func newMemStore(rows ...rejection) *memStore {
	s := &memStore{rows: map[string]rejection{}}
	for _, r := range rows {
		s.rows[memKey(r)] = r
	}
	return s
}

func memKey(r rejection) string { return r.Schema + "." + r.Table + "|" + r.Shape.hash() }

func (s *memStore) recent(_ context.Context, schema, table string, _ time.Duration,
	limit int) ([]rejection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	var out []rejection
	for _, r := range s.rows {
		if r.Schema == schema && r.Table == table && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *memStore) record(_ context.Context, r rejection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records++
	if s.recordErr != nil {
		return s.recordErr
	}
	if prev, ok := s.rows[memKey(r)]; ok {
		r.MeasureCount = prev.MeasureCount + 1
	} else {
		r.MeasureCount = 1
	}
	s.rows[memKey(r)] = r
	return nil
}

func (s *memStore) snapshot() []rejection {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]rejection, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out
}

var memNow = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// logRecorder captures logFn calls.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) log(component, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, component+": "+fmt.Sprintf(format, args...))
}

func (l *logRecorder) matching(fragments ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		ok := true
		for _, f := range fragments {
			ok = ok && strings.Contains(line, f)
		}
		if ok {
			out = append(out, line)
		}
	}
	return out
}

func defaultMemorySettings() memorySettings {
	return memorySettingsFrom(config.OptimizerRejectionMemoryConfig{})
}

func testMemory(store rejectionStore, logs *logRecorder) *rejectionMemory {
	logFn := noopLog2
	if logs != nil {
		logFn = logs.log
	}
	m := newRejectionMemory(store, defaultMemorySettings(), logFn)
	m.now = func() time.Time { return memNow }
	return m
}

// memTable is the analyzed table of the memory tests.
func memTable() TableContext {
	return TableContext{Schema: "public", Table: "ai_claims", LiveTuples: 10000,
		Queries: []QueryInfo{
			{QueryID: 1, Calls: 1000, MeanTimeMs: 5, TotalTimeMs: 5000},
			{QueryID: 2, Calls: 400, MeanTimeMs: 2, TotalTimeMs: 800},
		}}
}

func storedRejection(t *testing.T, ddl string, age time.Duration) rejection {
	t.Helper()
	tc := memTable()
	return rejection{Schema: tc.Schema, Table: tc.Table, Shape: mustShape(t, ddl), DDL: ddl,
		ImprovementPct: 0, MinImprovementPct: 10,
		Reason:   "call-weighted improvement 0.0% is below the 10.0% minimum",
		Workload: workloadOf(tc), RowEstimate: tc.LiveTuples,
		MeasuredAt: memNow.Add(-age), MeasureCount: 1}
}

func TestMemorySettings_DefaultsWithoutConfig(t *testing.T) {
	s := defaultMemorySettings()
	if s.MaxAge != 7*24*time.Hour || s.CallRatio != 2 || s.MeanRatio != 2 ||
		s.RowRatio != 2 || s.PromptMax != 5 {
		t.Fatalf("zero config settings = %+v, want 7d, 2x, 2x, 2x, 5 shapes", s)
	}
	d := memorySettingsFrom(config.DefaultConfig().LLM.Optimizer.RejectionMemory)
	if d != s {
		t.Fatalf("shipped defaults %+v differ from the zero-config fallback %+v", d, s)
	}
}

func TestMemorySettings_ConfiguredAndInvalidValues(t *testing.T) {
	s := memorySettingsFrom(config.OptimizerRejectionMemoryConfig{Enabled: true,
		MaxAgeDays: 3, CallVolumeRatio: 4, MeanTimeRatio: 1.5, RowEstimateRatio: 10,
		PromptMaxShapes: 2})
	if s.MaxAge != 72*time.Hour || s.CallRatio != 4 || s.MeanRatio != 1.5 ||
		s.RowRatio != 10 || s.PromptMax != 2 {
		t.Fatalf("configured settings = %+v", s)
	}
	bad := memorySettingsFrom(config.OptimizerRejectionMemoryConfig{MaxAgeDays: -1,
		CallVolumeRatio: 1, MeanTimeRatio: 0.5, RowEstimateRatio: math.NaN(),
		PromptMaxShapes: -3})
	if bad != defaultMemorySettings() {
		t.Fatalf("invalid values must fall back to defaults, got %+v", bad)
	}
	if c := memorySettingsFrom(config.OptimizerRejectionMemoryConfig{
		PromptMaxShapes: 500}); c.PromptMax != 20 {
		t.Fatalf("prompt shapes = %d, want the cap of 20", c.PromptMax)
	}
}

func TestMaterialChange_UnchangedWorkload(t *testing.T) {
	r := storedRejection(t, "CREATE INDEX i ON public.ai_claims (x)", time.Hour)
	if why := materialChange(r, memTable(), defaultMemorySettings(), memNow); why != "" {
		t.Fatalf("unchanged workload reported a change: %q", why)
	}
}

func TestMaterialChange_CallVolumeBoundary(t *testing.T) {
	cases := []struct {
		calls   int64
		changed bool
	}{{1990, false}, {2000, true}, {5000, true}, {510, false}, {500, true}, {0, true}}
	for _, c := range cases {
		r := storedRejection(t, "CREATE INDEX i ON public.ai_claims (x)", time.Hour)
		tc := memTable()
		tc.Queries[0].Calls = c.calls
		why := materialChange(r, tc, defaultMemorySettings(), memNow)
		if (why != "") != c.changed {
			t.Errorf("calls 1000 -> %d: change %q, want changed=%t", c.calls, why, c.changed)
		}
		if c.changed && !strings.Contains(why, "calls") {
			t.Errorf("calls 1000 -> %d: reason %q does not name the call volume", c.calls, why)
		}
	}
}

func TestMaterialChange_MeanTimeBoundary(t *testing.T) {
	cases := []struct {
		mean    float64
		changed bool
	}{{9.95, false}, {10, true}, {2.6, false}, {2.5, true}, {0, true}}
	for _, c := range cases {
		r := storedRejection(t, "CREATE INDEX i ON public.ai_claims (x)", time.Hour)
		tc := memTable()
		tc.Queries[0].MeanTimeMs = c.mean
		why := materialChange(r, tc, defaultMemorySettings(), memNow)
		if (why != "") != c.changed {
			t.Errorf("mean 5ms -> %.2fms: change %q, want changed=%t", c.mean, why, c.changed)
		}
		if c.changed && !strings.Contains(why, "mean") {
			t.Errorf("mean 5 -> %.2f: reason %q does not name the mean time", c.mean, why)
		}
	}
}

func TestMaterialChange_RowEstimateBoundary(t *testing.T) {
	cases := []struct {
		stored, now int64
		changed     bool
	}{{10000, 19999, false}, {10000, 20000, true}, {10000, 5001, false},
		{10000, 5000, true}, {0, 1, false}, {0, 2, true}, {0, 0, false}, {-5, 1, false}}
	for _, c := range cases {
		r := storedRejection(t, "CREATE INDEX i ON public.ai_claims (x)", time.Hour)
		r.RowEstimate = c.stored
		tc := memTable()
		tc.LiveTuples = c.now
		why := materialChange(r, tc, defaultMemorySettings(), memNow)
		if (why != "") != c.changed {
			t.Errorf("rows %d -> %d: change %q, want changed=%t", c.stored, c.now, why,
				c.changed)
		}
		if c.changed && !strings.Contains(why, "row") {
			t.Errorf("rows %d -> %d: reason %q does not name the rows", c.stored, c.now, why)
		}
	}
}

func TestMaterialChange_TargetQuerySet(t *testing.T) {
	r := storedRejection(t, "CREATE INDEX i ON public.ai_claims (x)", time.Hour)
	added := memTable()
	added.Queries = append(added.Queries, QueryInfo{QueryID: 3, Calls: 100, MeanTimeMs: 1})
	if why := materialChange(r, added, defaultMemorySettings(), memNow); !strings.Contains(
		why, "new target query 3") {
		t.Fatalf("a new target query must be a material change, got %q", why)
	}
	removed := memTable()
	removed.Queries = removed.Queries[:1]
	if why := materialChange(r, removed, defaultMemorySettings(), memNow); !strings.Contains(
		why, "query 2") {
		t.Fatalf("a vanished target query must be a material change, got %q", why)
	}
	noID := memTable()
	noID.Queries = append(noID.Queries, QueryInfo{QueryID: 0, Calls: 9999, MeanTimeMs: 99})
	if why := materialChange(r, noID, defaultMemorySettings(), memNow); why != "" {
		t.Fatalf("a query without a queryid has no identity to compare: %q", why)
	}
	empty := rejection{Shape: r.Shape, MeasuredAt: memNow, RowEstimate: 0}
	if why := materialChange(empty, TableContext{}, defaultMemorySettings(), memNow); why != "" {
		t.Fatalf("empty workload on both sides is unchanged: %q", why)
	}
}

func TestMaterialChange_MaxAgeBoundary(t *testing.T) {
	week := 7 * 24 * time.Hour
	for _, c := range []struct {
		age     time.Duration
		changed bool
	}{{week - time.Second, false}, {week, true}, {30 * 24 * time.Hour, true},
		{-time.Hour, false}} {
		r := storedRejection(t, "CREATE INDEX i ON public.ai_claims (x)", c.age)
		why := materialChange(r, memTable(), defaultMemorySettings(), memNow)
		if (why != "") != c.changed {
			t.Errorf("age %v: change %q, want changed=%t", c.age, why, c.changed)
		}
		if c.changed && !strings.Contains(why, "older than") {
			t.Errorf("age %v: reason %q does not say it expired", c.age, why)
		}
	}
}

func TestMaterialChange_ConfiguredThresholds(t *testing.T) {
	s := memorySettingsFrom(config.OptimizerRejectionMemoryConfig{MaxAgeDays: 1,
		CallVolumeRatio: 3, MeanTimeRatio: 3, RowEstimateRatio: 3, PromptMaxShapes: 5})
	r := storedRejection(t, "CREATE INDEX i ON public.ai_claims (x)", time.Hour)
	tc := memTable()
	tc.Queries[0].Calls, tc.Queries[0].MeanTimeMs, tc.LiveTuples = 2500, 12, 25000
	if why := materialChange(r, tc, s, memNow); why != "" {
		t.Fatalf("2.5x under a 3x threshold is not material: %q", why)
	}
	old := storedRejection(t, "CREATE INDEX i ON public.ai_claims (x)", 25*time.Hour)
	if why := materialChange(old, memTable(), s, memNow); why == "" {
		t.Fatal("a one-day max age must expire a 25-hour-old rejection")
	}
}

func TestTableMemory_SuppressesSameIdeaOnly(t *testing.T) {
	stored := storedRejection(t, lifeosDDL("ai_claims_evidence_pattern_idx", "id, status"),
		time.Hour)
	m := testMemory(newMemStore(stored), nil)
	v := m.view(context.Background(), memTable())
	hit, ok := v.suppress(Recommendation{DDL: lifeosDDL("ai_claims_evidence_prefix_idx", "id")})
	if !ok || hit.Shape.hash() != stored.Shape.hash() || v.skipped != 1 {
		t.Fatalf("same idea not suppressed: ok=%t skipped=%d", ok, v.skipped)
	}
	other := "CREATE INDEX CONCURRENTLY x ON public.ai_claims " +
		"(evidence_event_ids_json varchar_pattern_ops) INCLUDE (id)"
	if _, ok := v.suppress(Recommendation{DDL: other}); ok || v.skipped != 1 {
		t.Fatalf("a different opclass was suppressed: skipped=%d", v.skipped)
	}
	if _, ok := v.suppress(Recommendation{DDL: "not sql"}); ok {
		t.Fatal("an unparseable candidate must be evaluated, not suppressed")
	}
}

func TestTableMemory_MaterialChangeLiftsSuppression(t *testing.T) {
	stored := storedRejection(t, lifeosDDL("a", "id"), time.Hour)
	tc := memTable()
	tc.Queries[0].Calls *= 3
	v := testMemory(newMemStore(stored), nil).view(context.Background(), tc)
	if _, ok := v.suppress(Recommendation{DDL: lifeosDDL("b", "id")}); ok {
		t.Fatal("a rejection measured on a different workload must not suppress")
	}
	if lines := v.promptLines(); len(lines) != 0 {
		t.Fatalf("a stale rejection must not be fed to the model: %q", lines)
	}
	expired := storedRejection(t, lifeosDDL("a", "id"), 8*24*time.Hour)
	v = testMemory(newMemStore(expired), nil).view(context.Background(), memTable())
	if _, ok := v.suppress(Recommendation{DDL: lifeosDDL("b", "id")}); ok {
		t.Fatal("an expired rejection the store returned must not suppress")
	}
}

func TestTableMemory_OtherTableNeverMatches(t *testing.T) {
	stored := storedRejection(t, "CREATE INDEX i ON public.ai_claims (x)", time.Hour)
	tc := memTable()
	tc.Table = "ai_claims_archive"
	v := testMemory(newMemStore(stored), nil).view(context.Background(), tc)
	if _, ok := v.suppress(Recommendation{
		DDL: "CREATE INDEX i ON public.ai_claims_archive (x)"}); ok {
		t.Fatal("a rejection on another table suppressed this one")
	}
}

func TestTableMemory_NilMemoryIsInert(t *testing.T) {
	var m *rejectionMemory
	v := m.view(context.Background(), memTable())
	if v == nil {
		t.Fatal("a nil memory must still return a usable view")
	}
	if _, ok := v.suppress(Recommendation{DDL: lifeosDDL("a", "id")}); ok || v.skipped != 0 {
		t.Fatal("a nil memory suppressed a candidate")
	}
	if lines := v.promptLines(); lines != nil {
		t.Fatalf("nil memory prompt lines = %q", lines)
	}
	v.learn(storedRejection(t, lifeosDDL("a", "id"), 0))
	if _, ok := m.remember(context.Background(), memTable(),
		Recommendation{DDL: lifeosDDL("a", "id")}, 10); ok {
		t.Fatal("a nil memory reported a recorded rejection")
	}
}

func TestTableMemory_LoadErrorFailsOpenAndIsLogged(t *testing.T) {
	store := newMemStore(storedRejection(t, lifeosDDL("a", "id"), time.Hour))
	store.loadErr = errors.New("connection refused")
	logs := &logRecorder{}
	v := testMemory(store, logs).view(context.Background(), memTable())
	if _, ok := v.suppress(Recommendation{DDL: lifeosDDL("b", "id")}); ok {
		t.Fatal("a failed load must evaluate the candidate (fail open)")
	}
	got := logs.matching("WARN", "public.ai_claims", "connection refused")
	if len(got) != 1 {
		t.Fatalf("load error must be logged once with table and cause, got %q", logs.lines)
	}
}

func TestTableMemory_LearnSuppressesWithinCycle(t *testing.T) {
	v := testMemory(newMemStore(), nil).view(context.Background(), memTable())
	if _, ok := v.suppress(Recommendation{DDL: lifeosDDL("b", "id")}); ok {
		t.Fatal("empty memory suppressed a candidate")
	}
	v.learn(storedRejection(t, lifeosDDL("a", "id, status"), 0))
	if _, ok := v.suppress(Recommendation{DDL: lifeosDDL("b", "status")}); !ok {
		t.Fatal("a rejection learned this cycle must suppress the same idea")
	}
}

func TestTableMemory_PromptLinesBoundedAndNewestFirst(t *testing.T) {
	var rows []rejection
	for i := 0; i < 30; i++ {
		r := storedRejection(t, fmt.Sprintf("CREATE INDEX i%d ON public.ai_claims (c%d)", i, i),
			time.Duration(i+1)*time.Minute)
		r.ImprovementPct = float64(i) / 10
		rows = append(rows, r)
	}
	lines := testMemory(newMemStore(rows...), nil).view(context.Background(),
		memTable()).promptLines()
	if len(lines) != 5 {
		t.Fatalf("got %d prompt lines, want the default bound of 5", len(lines))
	}
	if !strings.Contains(lines[0], "btree (c0)") || !strings.Contains(lines[4], "btree (c4)") {
		t.Fatalf("lines are not newest first: %q", lines)
	}
	if !strings.Contains(lines[0], "0.0%") || !strings.Contains(lines[0], "10.0%") {
		t.Fatalf("line must carry the measured and minimum improvement: %q", lines[0])
	}
}

func TestTableMemory_PromptLineLengthBounded(t *testing.T) {
	long := "CREATE INDEX i ON public.ai_claims (x) WHERE " +
		strings.Repeat("a_very_long_column_name = 1 AND ", 40) + "z = 2"
	lines := testMemory(newMemStore(storedRejection(t, long, time.Minute)), nil).
		view(context.Background(), memTable()).promptLines()
	if len(lines) != 1 || len(lines[0]) > maxRejectionPromptLine {
		t.Fatalf("prompt line is %d chars, want at most %d", len(lines[0]),
			maxRejectionPromptLine)
	}
}

func TestRejectionMemory_RememberRecordsEvidence(t *testing.T) {
	store := newMemStore()
	m := testMemory(store, nil)
	rec := Recommendation{DDL: lifeosDDL("a", "id, status"), EstimatedImprovementPct: 0.4,
		WhatIf:       WhatIfRejected,
		WhatIfReason: "call-weighted improvement 0.4% is below the 10.0% minimum"}
	r, ok := m.remember(context.Background(), memTable(), rec, 10)
	if !ok || store.records != 1 {
		t.Fatalf("remember ok=%t records=%d", ok, store.records)
	}
	rows := store.snapshot()
	if len(rows) != 1 {
		t.Fatalf("%d rows stored", len(rows))
	}
	got := rows[0]
	if got.Schema != "public" || got.Table != "ai_claims" || got.DDL != rec.DDL ||
		got.ImprovementPct != 0.4 || got.MinImprovementPct != 10 ||
		got.Reason != rec.WhatIfReason || got.RowEstimate != 10000 ||
		!got.MeasuredAt.Equal(memNow) || got.Shape.hash() != r.Shape.hash() {
		t.Fatalf("stored evidence = %+v", got)
	}
	if len(got.Workload) != 2 || got.Workload[0] != (workloadSample{QueryID: 1, Calls: 1000,
		MeanMs: 5}) || got.Workload[1].QueryID != 2 {
		t.Fatalf("stored workload = %+v", got.Workload)
	}
}

func TestRejectionMemory_RememberRefusesBadInput(t *testing.T) {
	store := newMemStore()
	m := testMemory(store, nil)
	for name, ddl := range map[string]string{
		"unparseable": "CREATE INDEX i ON t (",
		"too long": "CREATE INDEX i ON public.ai_claims (x) WHERE " +
			strings.Repeat("x = 1 OR ", 1200) + "x = 2",
	} {
		if _, ok := m.remember(context.Background(), memTable(), Recommendation{DDL: ddl},
			10); ok {
			t.Errorf("%s DDL was remembered", name)
		}
	}
	if store.records != 0 {
		t.Fatalf("store received %d records for refused input", store.records)
	}
}

func TestRejectionMemory_RememberTruncatesLongReason(t *testing.T) {
	store := newMemStore()
	rec := Recommendation{DDL: lifeosDDL("a", "id"), WhatIfReason: strings.Repeat("r", 5000)}
	if _, ok := testMemory(store, nil).remember(context.Background(), memTable(), rec,
		10); !ok {
		t.Fatal("a long reason must be truncated, not refused")
	}
	if got := store.snapshot()[0].Reason; len(got) > maxRejectionReason {
		t.Fatalf("reason is %d chars, want at most %d", len(got), maxRejectionReason)
	}
}

func TestRejectionMemory_RecordErrorIsLoggedWithContext(t *testing.T) {
	store := newMemStore()
	store.recordErr = errors.New("permission denied for table optimizer_rejection")
	logs := &logRecorder{}
	if _, ok := testMemory(store, logs).remember(context.Background(), memTable(),
		Recommendation{DDL: lifeosDDL("a", "id")}, 10); ok {
		t.Fatal("a failed record reported success")
	}
	if got := logs.matching("WARN", "public.ai_claims", "permission denied"); len(got) != 1 {
		t.Fatalf("record failure must be logged with table and cause, got %q", logs.lines)
	}
}
