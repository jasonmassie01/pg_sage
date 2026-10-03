package policy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// The budget kind of a change comes from its typed action contract alone:
// never from evidence, the feature an LLM or caller names, or the SQL text
// of an unknown action. Unknown kinds are performance (fail closed).
func TestBudgetKindForClassifiesByActionTypeOnly(t *testing.T) {
	tests := []struct {
		name string
		req  ActionRequest
		want BudgetKind
	}{
		{"unused/redundant index drop", hygieneRequest("public.idx_a"), BudgetHygiene},
		{"vacuum", vacuumRequest("public.t"), BudgetHygiene},
		{"analyze", contractRequest("analyze_table", "ANALYZE public.t", "public.t"),
			BudgetHygiene},
		{"create index", perfRequest("public.t"), BudgetPerformance},
		{"reindex", contractRequest("reindex_concurrently",
			"REINDEX INDEX CONCURRENTLY public.i", "public.i"), BudgetPerformance},
		{"config", contractRequest("alter_system_guc", "ALTER SYSTEM SET work_mem = '8MB'",
			"instance"), BudgetPerformance},
		{"autovacuum tuning", contractRequest("set_table_autovacuum",
			"ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.05)", "public.t"),
			BudgetPerformance},
		{"revert of a created index", contractRequest("revert_created_index",
			"DROP INDEX CONCURRENTLY public.i", "public.i"), BudgetPerformance},
		{"unknown action", contractRequest("frobnicate", "VACUUM public.t", "public.t"),
			BudgetPerformance},
		{"empty action type", contractRequest("", "ANALYZE public.t", "public.t"),
			BudgetPerformance},
		{"no contract", ActionRequest{SQL: "ANALYZE public.t", Feature: "analyze"},
			BudgetPerformance},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BudgetKindFor(tt.req); got != tt.want {
				t.Fatalf("BudgetKindFor = %q, want %q", got, tt.want)
			}
		})
	}
	spoofed := perfRequest("public.t")
	spoofed.Feature = "vacuum"
	spoofed.Evidence = map[string]any{"budget_kind": "hygiene", "category": "unused_index"}
	if got := BudgetKindFor(spoofed); got != BudgetPerformance {
		t.Fatalf("evidence/feature claiming hygiene: kind = %q, want performance", got)
	}
}

// Hygiene spends only the hygiene budget and performance only its own: a
// full hygiene budget never parks an evidence-backed performance change.
func TestGateChargesEachKindToItsOwnBudget(t *testing.T) {
	freeAt := time.Date(2026, 7, 27, 1, 30, 0, 0, time.UTC)
	fx := &budgetFixture{doc: budgetDoc(), usage: map[BudgetKind]LimitUsage{
		BudgetHygiene:     {TablesInWindow: 4, TablesFreeAt: freeAt},
		BudgetPerformance: {TablesInWindow: 1},
	}}
	gate := newBudgetGate(t, fx)

	perf := gate.Authorize(context.Background(), perfRequest("public.events"))
	assertDecision(t, perf, VerdictExecute, ReasonAuthorized)
	if perf.BudgetKind != BudgetPerformance {
		t.Fatalf("performance decision kind = %q", perf.BudgetKind)
	}
	hygiene := gate.Authorize(context.Background(), hygieneRequest("test_x.idx_dup"))
	assertDecision(t, hygiene, VerdictPark, ReasonBlastRadiusExceeded)
	if hygiene.BudgetKind != BudgetHygiene ||
		!strings.Contains(hygiene.Detail, "hygiene budget") ||
		!strings.Contains(hygiene.Detail, freeAt.Format(time.RFC3339)) {
		t.Fatalf("hygiene park = %#v, want the hygiene budget and when it frees", hygiene)
	}

	fx.setUsage(BudgetPerformance, LimitUsage{TablesInWindow: 3, TablesFreeAt: freeAt})
	fx.setUsage(BudgetHygiene, LimitUsage{TablesInWindow: 1})
	assertDecision(t, gate.Authorize(context.Background(), hygieneRequest("test_x.idx_b")),
		VerdictExecute, ReasonAuthorized)
	parked := gate.Authorize(context.Background(), perfRequest("public.memories"))
	assertDecision(t, parked, VerdictPark, ReasonBlastRadiusExceeded)
	if !strings.Contains(parked.Detail, "performance budget") {
		t.Fatalf("performance park detail = %q, want the performance budget", parked.Detail)
	}
}

// Limits hold at exactly their value: tables and rows admit usage equal to
// the limit (usage already counts the request), changes do not (usage
// counts the changes before the request).
func TestKindBudgetBoundaries(t *testing.T) {
	type tc struct {
		name   string
		req    ActionRequest
		usage  LimitUsage
		edit   func(*Document)
		reason Reason
	}
	perf, hyg := perfRequest("public.t"), hygieneRequest("public.idx")
	zeroHygiene := func(doc *Document) { doc.BlastRadius.Hygiene = KindBudget{} }
	zeroRows := func(doc *Document) { doc.BlastRadius.MaxRowsRewritten = 0 }
	tests := []tc{
		{"perf tables at limit", perf, LimitUsage{TablesInWindow: 2}, nil, ReasonAuthorized},
		{"perf tables past limit", perf, LimitUsage{TablesInWindow: 3}, nil,
			ReasonBlastRadiusExceeded},
		{"perf changes below limit", perf, LimitUsage{SelfInitiatedChangesInWindow: 3}, nil,
			ReasonAuthorized},
		{"perf changes at limit", perf, LimitUsage{SelfInitiatedChangesInWindow: 4}, nil,
			ReasonRateLimitExceeded},
		{"hygiene tables at limit", hyg, LimitUsage{TablesInWindow: 3}, nil, ReasonAuthorized},
		{"hygiene tables past limit", hyg, LimitUsage{TablesInWindow: 4}, nil,
			ReasonBlastRadiusExceeded},
		{"hygiene changes below limit", hyg, LimitUsage{SelfInitiatedChangesInWindow: 4},
			nil, ReasonAuthorized},
		{"hygiene changes at limit", hyg, LimitUsage{SelfInitiatedChangesInWindow: 5}, nil,
			ReasonRateLimitExceeded},
		{"rows at limit", perf, LimitUsage{RowsRewritten: 100}, nil, ReasonAuthorized},
		{"rows past limit", perf, LimitUsage{RowsRewritten: 101}, nil,
			ReasonBlastRadiusExceeded},
		{"rows are shared by hygiene", hyg, LimitUsage{RowsRewritten: 101}, nil,
			ReasonBlastRadiusExceeded},
		{"zero hygiene, a table", hyg, LimitUsage{TablesInWindow: 1}, zeroHygiene,
			ReasonBlastRadiusExceeded},
		{"zero hygiene, no table", hyg, LimitUsage{}, zeroHygiene, ReasonAuthorized},
		{"zero rows, nothing rewritten", perf, LimitUsage{}, zeroRows, ReasonAuthorized},
		{"zero rows, one row rewritten", perf, LimitUsage{RowsRewritten: 1}, zeroRows,
			ReasonBlastRadiusExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := budgetDoc()
			if tt.edit != nil {
				tt.edit(&doc)
			}
			kind := BudgetKindFor(tt.req)
			fx := &budgetFixture{doc: doc, usage: map[BudgetKind]LimitUsage{kind: tt.usage}}
			got := newBudgetGate(t, fx).Authorize(context.Background(), tt.req)
			verdict := VerdictPark
			if tt.reason == ReasonAuthorized {
				verdict = VerdictExecute
			}
			assertDecision(t, got, verdict, tt.reason)
		})
	}
}

// The park detail names the full budget, its usage and limit, and when
// the window next frees room; a limit no expiry can satisfy says so.
func TestBudgetParkDetailSaysWhichBudgetAndWhenItFrees(t *testing.T) {
	at := time.Date(2026, 7, 27, 4, 5, 6, 0, time.UTC)
	tests := []struct {
		name  string
		req   ActionRequest
		usage LimitUsage
		edit  func(*Document)
		want  []string
	}{
		{"performance tables", perfRequest("public.t"),
			LimitUsage{TablesInWindow: 3, TablesFreeAt: at}, nil,
			[]string{"performance budget", "3 of 2 tables", at.Format(time.RFC3339)}},
		{"hygiene changes", hygieneRequest("public.i"),
			LimitUsage{SelfInitiatedChangesInWindow: 5, ChangesFreeAt: at}, nil,
			[]string{"hygiene budget", "5 of 5 changes", at.Format(time.RFC3339)}},
		{"rows rewritten", hygieneRequest("public.i"),
			LimitUsage{RowsRewritten: 150, RequestRowsRewritten: 60, RowsFreeAt: at}, nil,
			[]string{"rows rewritten", "150 of 100", at.Format(time.RFC3339)}},
		{"zero limit never frees", hygieneRequest("public.i"),
			LimitUsage{TablesInWindow: 1},
			func(doc *Document) { doc.BlastRadius.Hygiene = KindBudget{} },
			[]string{"hygiene budget", "1 of 0 tables", "until the policy limit is raised"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := budgetDoc()
			if tt.edit != nil {
				tt.edit(&doc)
			}
			fx := &budgetFixture{doc: doc,
				usage: map[BudgetKind]LimitUsage{BudgetKindFor(tt.req): tt.usage}}
			got := newBudgetGate(t, fx).Authorize(context.Background(), tt.req)
			if got.Verdict != VerdictPark {
				t.Fatalf("decision = %#v, want a park", got)
			}
			for _, part := range tt.want {
				if !strings.Contains(got.Detail, part) {
					t.Errorf("detail = %q, want it to contain %q", got.Detail, part)
				}
			}
			if recorded := fx.lastRecorded(t); recorded.Detail != got.Detail {
				t.Fatalf("recorded detail = %q, want %q", recorded.Detail, got.Detail)
			}
		})
	}
}

// Every budgeted decision carries its kind and the request's own row
// estimate, so the ledger can charge an execution to the right budget.
func TestGateStampsBudgetOnDecision(t *testing.T) {
	fx := &budgetFixture{doc: budgetDoc(), usage: map[BudgetKind]LimitUsage{
		BudgetPerformance: {RowsRewritten: 42, RequestRowsRewritten: 42},
	}}
	gate := newBudgetGate(t, fx)
	got := gate.Authorize(context.Background(), perfRequest("public.t"))
	assertDecision(t, got, VerdictExecute, ReasonAuthorized)
	if got.BudgetKind != BudgetPerformance || got.RowsRewritten != 42 {
		t.Fatalf("decision = %#v, want kind performance and 42 rows", got)
	}
	if recorded := fx.lastRecorded(t); recorded.BudgetKind != BudgetPerformance ||
		recorded.RowsRewritten != 42 {
		t.Fatalf("recorded = %#v, want kind performance and 42 rows", recorded)
	}
	readOnly := ActionRequest{Contract: &ActionContract{
		ActionType: "diagnose_lock_blockers", RiskTier: RiskReadOnly}}
	before := fx.usageCalls(BudgetPerformance)
	diag := gate.Authorize(context.Background(), readOnly)
	if diag.Verdict != VerdictExecute || diag.RowsRewritten != 0 ||
		fx.usageCalls(BudgetPerformance) != before {
		t.Fatalf("read-only = %#v (usage calls %d -> %d), want execute without usage",
			diag, before, fx.usageCalls(BudgetPerformance))
	}
}

// Two candidates racing for the last slot of a budget must not both pass:
// the read of usage and the recorded verdict are one critical section.
func TestGateSerializesTheLastBudgetSlot(t *testing.T) {
	ledger := &raceLedger{executed: map[string]bool{"public.a": true, "public.b": true}}
	doc := budgetDoc()
	doc.BlastRadius.MaxTablesPerWindow = 3
	now := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)
	gate := NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return newTestGateRuntime(), nil
		},
		ValidateSQL: func(string) error { return nil },
		Policy: func(context.Context, ActionRequest) (Document, error) {
			return doc, nil
		},
		Usage:                  ledger.usage,
		RecordDecisionDetailed: ledger.record,
		Now:                    func() time.Time { return now },
	})
	const racers = 12
	verdicts := make(chan Verdict, racers)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := 0; i < racers; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			req := perfRequest(fmt.Sprintf("public.racer_%d", i))
			verdicts <- gate.Authorize(context.Background(), req).Verdict
		}(i)
	}
	start.Done()
	done.Wait()
	close(verdicts)
	counts := map[Verdict]int{}
	for verdict := range verdicts {
		counts[verdict]++
	}
	if counts[VerdictExecute] != 1 || counts[VerdictPark] != racers-1 {
		t.Fatalf("verdicts = %v, want exactly one execute and %d parks", counts, racers-1)
	}
}

// ExplainBatch reads usage once per budget kind, so a hygiene family is
// explained against the hygiene budget, not the first request's.
func TestExplainBatchReadsUsageOncePerKind(t *testing.T) {
	fx := &budgetFixture{doc: budgetDoc(), usage: map[BudgetKind]LimitUsage{
		BudgetPerformance: {TablesInWindow: 9},
		BudgetHygiene:     {TablesInWindow: 1},
	}}
	gate := newBudgetGate(t, fx).(BatchExplainer)
	got := gate.ExplainBatch(context.Background(), []ActionRequest{
		perfRequest("public.a"), perfRequest("public.b"), hygieneRequest("public.i"),
	})
	if got[0].Verdict != VerdictPark || got[1].Verdict != VerdictPark ||
		got[2].Verdict != VerdictExecute {
		t.Fatalf("batch = %v/%v/%v, want park, park, execute",
			got[0].Verdict, got[1].Verdict, got[2].Verdict)
	}
	if fx.usageCalls(BudgetPerformance) != 1 || fx.usageCalls(BudgetHygiene) != 1 {
		t.Fatalf("usage reads = %d performance, %d hygiene; want one each",
			fx.usageCalls(BudgetPerformance), fx.usageCalls(BudgetHygiene))
	}
	if len(fx.recordedDecisions()) != 0 {
		t.Fatal("ExplainBatch recorded decisions")
	}
}

// A usage read failure fails closed with a distinguishable reason.
func TestGateBudgetUsageErrorBlocks(t *testing.T) {
	fx := &budgetFixture{doc: budgetDoc(), usageErr: fmt.Errorf("read policy usage: boom")}
	got := newBudgetGate(t, fx).Authorize(context.Background(), hygieneRequest("public.i"))
	if got.Verdict != VerdictBlocked || got.Reason != ReasonPolicyUnavailable ||
		!strings.Contains(got.Detail, "read policy usage") {
		t.Fatalf("decision = %#v, want blocked policy_unavailable naming the usage read", got)
	}
}

// budgetDoc: performance 2 tables / 4 changes, hygiene 3 tables / 5
// changes, 100 rows rewritten, windows always open.
func budgetDoc() Document {
	doc := UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.BlastRadius.MaxTablesPerWindow = 2
	doc.RateLimits.MaxSelfInitiatedChangesPerWindow = 4
	doc.BlastRadius.Hygiene = KindBudget{MaxTablesPerWindow: 3, MaxChangesPerWindow: 5}
	doc.BlastRadius.MaxRowsRewritten = 100
	return doc
}

func contractRequest(actionType, sql, target string) ActionRequest {
	return ActionRequest{
		Contract:   &ActionContract{ActionType: actionType, RiskTier: RiskSafe},
		SQL:        sql,
		TargetObjs: []string{target},
		Feature:    "index",
	}
}

func perfRequest(table string) ActionRequest {
	return contractRequest("create_index_concurrently",
		"CREATE INDEX CONCURRENTLY idx_budget ON "+table+" (a)", table)
}

func hygieneRequest(index string) ActionRequest {
	return contractRequest("drop_unused_index", "DROP INDEX CONCURRENTLY "+index, index)
}

func vacuumRequest(table string) ActionRequest {
	req := contractRequest("vacuum_table", "VACUUM "+table, table)
	req.Feature = "vacuum"
	return req
}

type budgetFixture struct {
	mu       sync.Mutex
	doc      Document
	usage    map[BudgetKind]LimitUsage
	usageErr error
	calls    map[BudgetKind]int
	recorded []Decision
}

func (fx *budgetFixture) setUsage(kind BudgetKind, usage LimitUsage) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.usage[kind] = usage
}

func (fx *budgetFixture) usageCalls(kind BudgetKind) int {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return fx.calls[kind]
}

func (fx *budgetFixture) recordedDecisions() []Decision {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return append([]Decision(nil), fx.recorded...)
}

func (fx *budgetFixture) lastRecorded(t *testing.T) Decision {
	t.Helper()
	recorded := fx.recordedDecisions()
	if len(recorded) == 0 {
		t.Fatal("no decision was recorded")
	}
	return recorded[len(recorded)-1]
}

func newBudgetGate(t *testing.T, fx *budgetFixture) Gate {
	t.Helper()
	return NewGate(GateConfig{
		Runtime: func(context.Context, ActionRequest) (RuntimeState, error) {
			return newTestGateRuntime(), nil
		},
		ValidateSQL: func(string) error { return nil },
		Policy: func(context.Context, ActionRequest) (Document, error) {
			return fx.doc, nil
		},
		Usage: func(_ context.Context, req ActionRequest) (LimitUsage, error) {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			if fx.calls == nil {
				fx.calls = map[BudgetKind]int{}
			}
			kind := BudgetKindFor(req)
			fx.calls[kind]++
			return fx.usage[kind], fx.usageErr
		},
		RecordDecisionDetailed: func(
			_ context.Context, _ ActionRequest, decision Decision,
		) (string, int64, error) {
			fx.mu.Lock()
			defer fx.mu.Unlock()
			fx.recorded = append(fx.recorded, decision)
			return fmt.Sprintf("ev-%d", len(fx.recorded)), int64(len(fx.recorded)), nil
		},
		Now: func() time.Time { return time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC) },
	})
}

// raceLedger is a shared window: usage counts the executed tables plus
// the request's own, and an execute verdict spends its table when it is
// recorded. The pause between the read and the verdict widens the race.
type raceLedger struct {
	mu       sync.Mutex
	executed map[string]bool
}

func (l *raceLedger) usage(_ context.Context, req ActionRequest) (LimitUsage, error) {
	l.mu.Lock()
	tables := int64(len(l.executed))
	if !l.executed[req.TargetObjs[0]] {
		tables++
	}
	l.mu.Unlock()
	time.Sleep(3 * time.Millisecond)
	return LimitUsage{TablesInWindow: tables}, nil
}

func (l *raceLedger) record(
	_ context.Context, req ActionRequest, decision Decision,
) (string, int64, error) {
	if decision.Verdict == VerdictExecute {
		l.mu.Lock()
		l.executed[req.TargetObjs[0]] = true
		l.mu.Unlock()
	}
	return "ev-race", 1, nil
}
