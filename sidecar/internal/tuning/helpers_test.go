package tuning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/tuner"
)

// Shared fixtures and fakes for the tuning agent's unit tests. The fakes
// implement the agent's dependency interfaces; every DB-backed
// implementation also has a real-PostgreSQL test (*_db_test.go).

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func noLog(string, string, ...any) {}

// logSink records log lines for assertions.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) fn(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *logSink) contains(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			return true
		}
	}
	return false
}

// stmt is a pg_stat_statements row.
func stmt(id int64, text string, calls int64, totalMs float64) collector.QueryStats {
	mean := 0.0
	if calls > 0 {
		mean = totalMs / float64(calls)
	}
	return collector.QueryStats{QueryID: id, Query: text, Calls: calls,
		TotalExecTime: totalMs, MeanExecTime: mean}
}

func table(schema, name string, live, dead int64) collector.TableStats {
	return collector.TableStats{SchemaName: schema, RelName: name, NLiveTup: live,
		NDeadTup: dead, TableBytes: 64 << 20, TotalBytes: 80 << 20, Relpersistence: "p"}
}

func index(schema, tbl, name, def string, scans int64) collector.IndexStats {
	return collector.IndexStats{SchemaName: schema, RelName: tbl, IndexRelName: name,
		IdxScan: scans, IsValid: true, IndexDef: def, IndexType: "btree",
		IndexBytes: 8 << 20}
}

func snapAt(at time.Time, qs []collector.QueryStats, ts []collector.TableStats,
	is []collector.IndexStats) *collector.Snapshot {
	return &collector.Snapshot{CollectedAt: at, Queries: qs, Tables: ts, Indexes: is}
}

// ordersPair is a two-snapshot interval in which one statement on
// public.orders dominates the workload time.
func ordersPair() (prev, cur *collector.Snapshot) {
	tbl := []collector.TableStats{table("public", "orders", 1_000_000, 1000)}
	idx := []collector.IndexStats{index("public", "orders", "orders_pkey",
		"CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)", 900)}
	idx[0].IsUnique, idx[0].IsPrimary = true, true
	prev = snapAt(t0, []collector.QueryStats{
		stmt(101, "SELECT * FROM public.orders WHERE customer_id = $1", 1000, 10000),
		stmt(102, "SELECT * FROM public.orders WHERE id = $1", 5000, 500),
	}, tbl, idx)
	cur = snapAt(t0.Add(5*time.Minute), []collector.QueryStats{
		stmt(101, "SELECT * FROM public.orders WHERE customer_id = $1", 1600, 16000),
		stmt(102, "SELECT * FROM public.orders WHERE id = $1", 5600, 600),
	}, tbl, idx)
	// Statement 101 sorts in work_mem: 4800 temp blocks in the interval.
	prev.Queries[0].TempBlksWritten, cur.Queries[0].TempBlksWritten = 200, 5000
	return prev, cur
}

func confirmedFact(id int64, typ facts.Type, kind facts.Kind, subject string,
	value map[string]string) facts.Fact {
	return facts.Fact{ID: id, Type: typ, Kind: kind, Subject: subject, Value: value,
		Status: facts.StatusConfirmed, Source: facts.SourceOperator,
		DecidedBy: "alice@example.com", DecidedAt: &t0, CreatedAt: t0, UpdatedAt: t0}
}

func pct(v float64) *float64 { return &v }

// ---- fake model ----

// scriptedModel replies with one scripted turn per call; it records the
// messages and options it received.
type scriptedModel struct {
	mu      sync.Mutex
	turns   []func(msgs []llm.Message) (llm.ToolResult, error)
	calls   int
	msgs    [][]llm.Message
	opts    []llm.ToolOptions
	tools   [][]llm.ToolSpec
	reqHook func(n int)
}

func (m *scriptedModel) ChatWithTools(_ context.Context, msgs []llm.Message,
	tools []llm.ToolSpec, opts llm.ToolOptions) (llm.ToolResult, error) {
	m.mu.Lock()
	n := m.calls
	m.calls++
	m.msgs = append(m.msgs, append([]llm.Message(nil), msgs...))
	m.opts = append(m.opts, opts)
	m.tools = append(m.tools, tools)
	hook := m.reqHook
	var turn func([]llm.Message) (llm.ToolResult, error)
	if n < len(m.turns) {
		turn = m.turns[n]
	}
	m.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	if opts.Budget != nil {
		if !opts.Budget.CanSpend(100) {
			return llm.ToolResult{}, llm.ErrBudgetExhausted
		}
		opts.Budget.Spend(100)
	}
	if turn == nil {
		return llm.ToolResult{Content: `{"proposals":[]}`, Tokens: 100}, nil
	}
	return turn(msgs)
}

func (m *scriptedModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func answer(content string) func([]llm.Message) (llm.ToolResult, error) {
	return func([]llm.Message) (llm.ToolResult, error) {
		return llm.ToolResult{Content: content, Tokens: 100}, nil
	}
}

func toolCall(name, args string) func([]llm.Message) (llm.ToolResult, error) {
	return func([]llm.Message) (llm.ToolResult, error) {
		return llm.ToolResult{Tokens: 100, ToolCalls: []llm.ToolCall{{
			ID: "call_" + name, Name: name, Arguments: json.RawMessage(args)}}}, nil
	}
}

func failing(err error) func([]llm.Message) (llm.ToolResult, error) {
	return func([]llm.Message) (llm.ToolResult, error) { return llm.ToolResult{}, err }
}

// proposalsJSON renders an answer with the given proposals.
func proposalsJSON(t *testing.T, ps ...map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"proposals": ps})
	if err != nil {
		t.Fatalf("marshal proposals: %v", err)
	}
	return string(raw)
}

// ---- fake index tools (optimizer) ----

type fakeIndexes struct {
	mu        sync.Mutex
	cold      bool
	contexts  map[string]optimizer.TableContext
	admit     func(rec optimizer.Recommendation) optimizer.Admission
	admitted  []string
	measured  []string
	ctxErr    error
	whatIfSkp int64
}

func newFakeIndexes() *fakeIndexes {
	return &fakeIndexes{contexts: map[string]optimizer.TableContext{}}
}

func (f *fakeIndexes) ColdStart(context.Context) bool { return f.cold }

func (f *fakeIndexes) TableContext(_ context.Context, _ *collector.Snapshot, tbl string) (
	optimizer.TableContext, bool, error) {
	if f.ctxErr != nil {
		return optimizer.TableContext{}, false, f.ctxErr
	}
	tc, ok := f.contexts[tbl]
	return tc, ok, nil
}

func (f *fakeIndexes) Admit(_ context.Context, rec optimizer.Recommendation,
	tc optimizer.TableContext) optimizer.Admission {
	f.mu.Lock()
	f.admitted = append(f.admitted, rec.DDL)
	f.mu.Unlock()
	if f.admit != nil {
		return f.admit(rec)
	}
	rec.Table = tc.Schema + "." + tc.Table
	rec.Category = optimizer.OptimizerCategory
	rec.WhatIf, rec.Validated = optimizer.WhatIfVerified, true
	rec.EstimatedImprovementPct = 40
	for _, q := range tc.Queries {
		rec.AffectedQueryIDs = append(rec.AffectedQueryIDs, q.QueryID)
	}
	return optimizer.Admission{Rec: rec, Outcome: optimizer.AdmitAccepted}
}

func (f *fakeIndexes) MeasuredRejections(context.Context, optimizer.TableContext) []string {
	return f.measured
}

func (f *fakeIndexes) MemoryStats() optimizer.MemoryStats {
	return optimizer.MemoryStats{WhatIfSkipped: f.whatIfSkp}
}

func (f *fakeIndexes) admittedDDL() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.admitted...)
}

func ordersContext() optimizer.TableContext {
	return optimizer.TableContext{Schema: "public", Table: "orders", LiveTuples: 1_000_000,
		Columns: []optimizer.ColumnInfo{{Name: "id", Type: "bigint"},
			{Name: "customer_id", Type: "bigint"}, {Name: "status", Type: "text"},
			{Name: "created_at", Type: "timestamptz"}},
		Indexes: []optimizer.IndexInfo{{Name: "orders_pkey", IsUnique: true, IsValid: true,
			Definition: "CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)"}},
		Queries: []optimizer.QueryInfo{{QueryID: 101,
			Text: "SELECT * FROM public.orders WHERE customer_id = $1", Calls: 1600,
			MeanTimeMs: 10, TotalTimeMs: 16000}},
		WriteRateKnown: true}
}

// ---- fake facts ----

type fakeFacts struct {
	list []facts.Fact
	err  error
	n    int
}

func (f *fakeFacts) List(_ context.Context, filter facts.Filter) ([]facts.Fact, error) {
	f.n++
	if f.err != nil {
		return nil, f.err
	}
	var out []facts.Fact
	for _, fact := range f.list {
		if len(filter.Status) == 0 {
			out = append(out, fact)
			continue
		}
		for _, s := range filter.Status {
			if fact.Status == s {
				out = append(out, fact)
				break
			}
		}
	}
	return out, nil
}

// ---- fake hint sink (tuner) ----

type fakeHints struct {
	available bool
	err       error
	recordErr error
	checked   []tuner.HintProposal
	recorded  []tuner.HintProposal
}

func (h *fakeHints) HintsAvailable() bool { return h.available }

func (h *fakeHints) CheckHint(_ context.Context, p tuner.HintProposal) (
	analyzer.Finding, error) {
	if h.err != nil {
		return analyzer.Finding{}, h.err
	}
	h.checked = append(h.checked, p)
	detail := map[string]any{"queryid": p.QueryID, "hint_directive": p.Hint}
	for k, v := range p.Detail {
		detail[k] = v
	}
	return analyzer.Finding{Category: "query_tuning", Severity: "warning",
		ObjectType: "query", ObjectIdentifier: fmt.Sprintf("queryid:%d", p.QueryID),
		Title: "Per-query tuning", Detail: detail, Recommendation: p.Rationale,
		RecommendedSQL: tuner.BuildInsertSQL(p.QueryID, p.Hint),
		RollbackSQL:    tuner.BuildDeleteSQL(p.QueryID), ActionRisk: "safe"}, nil
}

func (h *fakeHints) RecordHint(_ context.Context, p tuner.HintProposal) error {
	if h.recordErr != nil {
		return h.recordErr
	}
	h.recorded = append(h.recorded, p)
	return nil
}

// ---- fake store ----

type fakeStore struct {
	mu        sync.Mutex
	open      []analyzer.Finding
	openErr   error
	rejected  map[string]bool
	rejErr    error
	outcomes  []OutcomeSample
	outErr    error
	outCalls  int
	plans     map[int64]Plan
	extStats  []ExtStat
	colStats  []ColumnStat
	openCalls int
	catalog   *CatalogState // nil: every table and index exists, no index defs
	catErr    error
	actions   []SettingAction
	actErr    error
	day       map[string][2]int64 // UTC day -> tokens, requests charged
	dayErr    error
	chargeErr error
}

func (s *fakeStore) DayBudgetUsed(_ context.Context, day time.Time) (int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dayErr != nil {
		return 0, 0, s.dayErr
	}
	u := s.day[day.UTC().Format(time.DateOnly)]
	return u[0], u[1], nil
}

func (s *fakeStore) ChargeDayBudget(_ context.Context, day time.Time, tokens,
	requests int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chargeErr != nil {
		return s.chargeErr
	}
	if s.day == nil {
		s.day = map[string][2]int64{}
	}
	k := day.UTC().Format(time.DateOnly)
	u := s.day[k]
	s.day[k] = [2]int64{u[0] + tokens, u[1] + requests}
	return nil
}

func (s *fakeStore) SettingActions(_ context.Context, since time.Time) (
	[]SettingAction, error) {
	if s.actErr != nil {
		return nil, s.actErr
	}
	var out []SettingAction
	for _, a := range s.actions {
		if !a.ExecutedAt.Before(since) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *fakeStore) Relations(_ context.Context, tables, indexes []string) (
	CatalogState, error) {
	if s.catErr != nil {
		return CatalogState{}, s.catErr
	}
	if s.catalog != nil {
		return *s.catalog, nil
	}
	st := CatalogState{Tables: map[string]bool{}, Indexes: map[string]bool{}}
	for _, t := range tables {
		st.Tables[t] = true
	}
	for _, i := range indexes {
		st.Indexes[i] = true
	}
	return st, nil
}

func (s *fakeStore) OpenFindings(_ context.Context, cats []string) ([]analyzer.Finding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openCalls++
	return s.open, s.openErr
}

func (s *fakeStore) OperatorRejected(context.Context, time.Time) (map[string]bool, error) {
	return s.rejected, s.rejErr
}

func (s *fakeStore) Outcomes(_ context.Context, classes []string, _ time.Time, _ int) (
	[]OutcomeSample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outCalls++
	if s.outErr != nil {
		return nil, s.outErr
	}
	var out []OutcomeSample
	for _, o := range s.outcomes {
		for _, c := range classes {
			if o.Class == c {
				out = append(out, o)
			}
		}
	}
	return out, nil
}

func (s *fakeStore) Plan(_ context.Context, id int64, _ string) (Plan, error) {
	if p, ok := s.plans[id]; ok {
		return p, nil
	}
	return Plan{Source: PlanSourceNone}, nil
}

func (s *fakeStore) ExtendedStats(context.Context, string, string) ([]ExtStat, error) {
	return s.extStats, nil
}

func (s *fakeStore) ColumnStats(context.Context, string, string, []string) (
	[]ColumnStat, error) {
	return s.colStats, nil
}

var errFake = errors.New("fake failure")

// improvedOutcomes are n decided outcomes of a class/method predicted at
// predicted%, hits of which improved.
func improvedOutcomes(class, method string, predicted float64, n, hits int) []OutcomeSample {
	out := make([]OutcomeSample, 0, n)
	for i := 0; i < n; i++ {
		verdict := "neutral"
		if i < hits {
			verdict = "improved"
		}
		out = append(out, OutcomeSample{Class: class, Method: method,
			PredictedPct: predicted, Verdict: verdict, Tolerance: "met"})
	}
	return out
}

// ---- agent construction ----

type harness struct {
	agent   *Agent
	model   *scriptedModel
	indexes *fakeIndexes
	facts   *fakeFacts
	hints   *fakeHints
	store   *fakeStore
	logs    *logSink
}

func defaultSettings() Settings {
	s := Settings{
		ConfidenceThreshold: 0.5,
		Allowed: map[ProposalType]bool{ProposeIndexCreate: true, ProposeIndexDrop: true,
			ProposeGUC: true, ProposeReloption: true, ProposeStatistics: true,
			ProposeQueryHint: true},
		MaxOutputTokens: 4096,
		Thresholds:      DefaultThresholds(),
	}
	s.Tuning.Enabled = true
	s.Tuning.MaxCasesPerCycle = 3
	s.Tuning.MaxRequestsPerCycle = 12
	s.Tuning.MaxTokensPerCycle = 60000
	s.Tuning.MaxTurnsPerCase = 6
	s.Tuning.MaxProposalsPerCycle = 10
	s.Tuning.CalibrationMinOutcomes = 5
	s.Tuning.CalibrationWindowDays = 180
	s.Memory.Enabled = true
	s.Memory.MaxAgeDays = 7
	s.Memory.CallVolumeRatio, s.Memory.MeanTimeRatio, s.Memory.RowEstimateRatio = 2, 2, 2
	s.Memory.SkipLLMAfter = 3
	s.Memory.PromptMaxShapes = 5
	return s
}

func newHarness(t *testing.T, turns ...func([]llm.Message) (llm.ToolResult, error)) *harness {
	t.Helper()
	return newHarnessWith(t, defaultSettings(), turns...)
}

func newHarnessWith(t *testing.T, s Settings,
	turns ...func([]llm.Message) (llm.ToolResult, error)) *harness {
	t.Helper()
	h := &harness{model: &scriptedModel{turns: turns}, indexes: newFakeIndexes(),
		facts: &fakeFacts{}, hints: &fakeHints{available: true},
		store: &fakeStore{rejected: map[string]bool{}}, logs: &logSink{}}
	h.indexes.contexts["public.orders"] = ordersContext()
	h.agent = New(s, Deps{Model: h.model, Indexes: h.indexes, Facts: h.facts,
		Hints: h.hints, Store: h.store, Now: func() time.Time { return t0 }}, h.logs.fn)
	return h
}

func findingByCategory(fs []analyzer.Finding, cat string) (analyzer.Finding, bool) {
	for _, f := range fs {
		if f.Category == cat {
			return f, true
		}
	}
	return analyzer.Finding{}, false
}
