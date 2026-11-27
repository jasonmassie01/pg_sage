package tuner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// The tuning agent's per-query hints go through the tuner (roadmap 2.2):
// the tuner keeps its pg_hint_plan validation, its Set() allowlist and
// clamp, its sage.query_hints bookkeeping and its cooldown, and the
// tuner's own deterministic pass then leaves that statement alone.
// CheckHint has no side effects; only RecordHint, called for the hints
// that survive the agent's per-cycle cap, records and cools down.

func hintTuner(available bool) *Tuner {
	hp := &HintPlanAvailability{ApplicationEffect: available, HintTableReady: available}
	return New(nil, TunerConfig{WorkMemMaxMB: 256, CascadeCooldownCycles: 3}, hp,
		noopLogFn)
}

func agentHint(hint string) HintProposal {
	return HintProposal{QueryID: 4242, Query: "SELECT * FROM orders WHERE customer_id = $1",
		Hint: hint, Rationale: "the planner prefers a sequential scan",
		Detail: map[string]any{"producer": "tuning_agent", "case_id": "top_statement:4242"}}
}

func TestCheckHint_BuildsTheFinding(t *testing.T) {
	tu := hintTuner(true)
	if !tu.HintsAvailable() {
		t.Fatal("pg_hint_plan with its hint table is available")
	}
	f, err := tu.CheckHint(context.Background(), agentHint("IndexScan(orders orders_c_idx)"))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if f.Category != "query_tuning" || f.ObjectIdentifier != "queryid:4242" ||
		f.RecommendedSQL != BuildInsertSQL(4242, "IndexScan(orders orders_c_idx)") ||
		f.RollbackSQL != BuildDeleteSQL(4242, "IndexScan(orders orders_c_idx)") {
		t.Fatalf("finding = %+v", f)
	}
	if f.Detail["hint_directive"] != "IndexScan(orders orders_c_idx)" ||
		f.Detail["case_id"] != "top_statement:4242" || f.Detail["queryid"] != int64(4242) ||
		f.Recommendation != "the planner prefers a sequential scan" {
		t.Fatalf("detail = %v rec %q", f.Detail, f.Recommendation)
	}
	if _, cooling := tu.recentlyTuned[4242]; cooling {
		t.Fatal("a checked hint leaves no trace: nothing cools down until it is recorded")
	}
	err = tu.RecordHint(context.Background(), agentHint("IndexScan(orders orders_c_idx)"))
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, cooling := tu.recentlyTuned[4242]; !cooling {
		t.Fatal("a recorded hint cools down: the tuner's own pass skips it this cycle")
	}
}

func TestCheckHint_ClampsWorkMem(t *testing.T) {
	tu := hintTuner(true)
	f, err := tu.CheckHint(context.Background(), agentHint(`Set(work_mem "2GB") HashJoin(o c)`))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if got := f.Detail["hint_directive"].(string); !strings.Contains(got,
		`Set(work_mem "256MB")`) {
		t.Fatalf("hint = %q: work_mem is clamped to tuner.work_mem_max_mb", got)
	}
}

func TestCheckHint_Refusals(t *testing.T) {
	ctx := context.Background()
	if _, err := hintTuner(false).CheckHint(ctx, agentHint("SeqScan(orders)")); !errors.Is(
		err, ErrHintsUnavailable) {
		t.Fatalf("no pg_hint_plan: %v", err)
	}
	if err := hintTuner(false).RecordHint(ctx, agentHint("SeqScan(orders)")); !errors.Is(
		err, ErrHintsUnavailable) {
		t.Fatalf("no pg_hint_plan, nothing recorded: %v", err)
	}
	tu := hintTuner(true)
	for _, bad := range []string{"", "DROP TABLE orders", "SeqScan(orders); DELETE FROM x",
		`Set(statement_timeout "0")`, "Leading((a b))"} {
		if _, err := tu.CheckHint(ctx, agentHint(bad)); !errors.Is(err, ErrInvalidHint) {
			t.Fatalf("%q: %v", bad, err)
		}
		if err := tu.RecordHint(ctx, agentHint(bad)); !errors.Is(err, ErrInvalidHint) {
			t.Fatalf("record %q: %v", bad, err)
		}
	}
	if _, err := tu.CheckHint(ctx, HintProposal{Hint: "SeqScan(orders)"}); !errors.Is(err,
		ErrInvalidHint) {
		t.Fatalf("a hint needs a statement: %v", err)
	}
	if len(tu.recentlyTuned) != 0 {
		t.Fatal("a refused hint does not cool anything down")
	}
	if _, err := tu.CheckHint(ctx, agentHint("SeqScan(orders)")); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if _, err := tu.CheckHint(ctx, agentHint("IndexScan(orders)")); err != nil {
		t.Fatalf("checking twice records nothing, so the second check passes: %v", err)
	}
	if err := tu.RecordHint(ctx, agentHint("SeqScan(orders)")); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if _, err := tu.CheckHint(ctx, agentHint("IndexScan(orders)")); !errors.Is(err,
		ErrHintExists) {
		t.Fatalf("one hint per statement while it is proposed or cooling: %v", err)
	}
	if err := tu.RecordHint(ctx, agentHint("IndexScan(orders)")); !errors.Is(err,
		ErrHintExists) {
		t.Fatalf("a second record of the statement is refused: %v", err)
	}
	var nilTuner *Tuner
	if nilTuner.HintsAvailable() {
		t.Fatal("a nil tuner offers no hints")
	}
}

func TestRecordHint_RecordsOnlyWhenAsked(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	// The tuner below claims a ready hint table, and since tuning-02 every
	// check reads it: that claim holds only for the pg_hint_plan 1.7 layout.
	requireQueryIDHintTable(t, pool)
	if _, err := pool.Exec(ctx, "DELETE FROM sage.query_hints WHERE queryid = 4242"); err != nil {
		t.Fatalf("clean: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM sage.query_hints WHERE queryid = 4242") })
	tu := New(pool, TunerConfig{WorkMemMaxMB: 256}, &HintPlanAvailability{ApplicationEffect: true,
		HintTableReady: true}, noopLogFn)
	if _, err := tu.CheckHint(ctx, agentHint("SeqScan(orders)")); err != nil {
		t.Fatalf("check: %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.query_hints
		WHERE queryid = 4242`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("a checked hint writes nothing: %d rows, %v", rows, err)
	}
	if err := tu.RecordHint(ctx, agentHint("SeqScan(orders)")); err != nil {
		t.Fatalf("record: %v", err)
	}
	var hint, symptom, status string
	if err := pool.QueryRow(ctx, `SELECT hint_text, symptom, status FROM sage.query_hints
		WHERE queryid = 4242`).Scan(&hint, &symptom, &status); err != nil {
		t.Fatalf("query_hints row: %v", err)
	}
	if hint != "SeqScan(orders)" || symptom != "tuning_agent" || status != "proposed" {
		t.Fatalf("row = %q %q %q", hint, symptom, status)
	}
	other := New(pool, TunerConfig{WorkMemMaxMB: 256},
		&HintPlanAvailability{ApplicationEffect: true, HintTableReady: true}, noopLogFn)
	if _, err := other.CheckHint(ctx, agentHint("IndexScan(orders)")); !errors.Is(err,
		ErrHintExists) {
		t.Fatalf("a proposed hint survives a restart (loaded from sage.query_hints): %v", err)
	}
}

// tuning-03: agent hints skip no memory guard. Above 5% of host memory a
// work_mem hint is refused; with host memory unknown it needs an operator.

func hostTuner(maxMB int, hostBytes int64) *Tuner {
	tu := New(nil, TunerConfig{WorkMemMaxMB: maxMB, CascadeCooldownCycles: 3},
		&HintPlanAvailability{ApplicationEffect: true, HintTableReady: true}, noopLogFn)
	tu.WithHostMemory(hostOf(hostBytes))
	return tu
}

func TestCheckHint_RefusesWorkMemAboveFivePercentOfHost(t *testing.T) {
	ctx := context.Background()
	for _, maxMB := range []int{256, 1024} {
		tu := hostTuner(maxMB, 4<<30)
		_, err := tu.CheckHint(ctx, agentHint(`Set(work_mem "512MB")`))
		if !errors.Is(err, ErrHintMemory) ||
			!strings.Contains(err.Error(), "above 5% of host memory") {
			t.Fatalf("max %d: err = %v, want a memory refusal", maxMB, err)
		}
		if err := tu.RecordHint(ctx, agentHint(`Set(work_mem "512MB")`)); !errors.Is(err,
			ErrHintMemory) {
			t.Fatalf("max %d: record err = %v", maxMB, err)
		}
		if len(tu.recentlyTuned) != 0 {
			t.Fatalf("a refused hint cools nothing down: %v", tu.recentlyTuned)
		}
	}
}

func TestCheckHint_HostCapBoundary(t *testing.T) {
	// 5% of 4 GiB is 214,748,364 bytes: 204 MB fits, 205 MB does not.
	ctx := context.Background()
	tu := hostTuner(1024, 4<<30)
	f, err := tu.CheckHint(ctx, agentHint(`Set(work_mem "204MB") HashJoin(o c)`))
	if err != nil {
		t.Fatalf("204MB on a 4 GiB host: %v", err)
	}
	if _, gated := f.Detail[analyzer.DetailApprovalRequired]; gated {
		t.Fatalf("a known host within the cap needs no approval: %v", f.Detail)
	}
	b, _ := f.Detail["work_mem_budget"].(map[string]any)
	if b["hint_mb"] != 204 || b["ceiling_mb"] != 204 || b["host_memory_mib"] != int64(4096) {
		t.Fatalf("budget detail = %v", f.Detail["work_mem_budget"])
	}
	if _, err := tu.CheckHint(ctx, agentHint(`Set(work_mem "205MB")`)); !errors.Is(err,
		ErrHintMemory) {
		t.Fatalf("205MB on a 4 GiB host: %v", err)
	}
}

func TestCheckHint_UnknownHostMemoryNeedsApproval(t *testing.T) {
	ctx := context.Background()
	for name, tu := range map[string]*Tuner{"no source": hintTuner(true),
		"source unknown": hostTuner(256, 0)} {
		f, err := tu.CheckHint(ctx, agentHint(`Set(work_mem "512MB")`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		why, _ := f.Detail[analyzer.DetailApprovalRequired].(string)
		if !strings.Contains(why, "host memory is unknown") {
			t.Fatalf("%s: approval_required = %q", name, why)
		}
		if f.Detail["hint_directive"] != `Set(work_mem "256MB")` {
			t.Fatalf("%s: still clamped to tuner.work_mem_max_mb: %v", name,
				f.Detail["hint_directive"])
		}
		g, err := tu.CheckHint(ctx, agentHint("IndexScan(orders orders_c_idx)"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, gated := g.Detail[analyzer.DetailApprovalRequired]; gated {
			t.Fatalf("%s: a hint without work_mem is not gated on memory: %v", name, g.Detail)
		}
	}
}
