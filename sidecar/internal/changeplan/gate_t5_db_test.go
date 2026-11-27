package changeplan

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// G-T5, local (plan-02e, CHECK-19 and CHECK-20): a three-step plan on a
// real executor and real DDL, approved all at once, with a workload
// running a window before each step. Steps 1 and 2 index t1(a) and t2(b);
// their reads improve and they are kept. The sidecar is killed between
// step 2's execution and its verdict and a new engine (another sidecar)
// takes over: step 2 never runs twice. Step 3 indexes t3(payload): its
// point reads improve, but the narrow, write-heavy table's WAL per insert
// rises past the verifier's write_impact_pct, so the verifier's
// write_impact rule (which fires only on an index whose queries improved)
// rolls it back as regressed: the plan records it reverted_by_verifier and
// halts with one notification. The verdicts are recorded through the
// verifier's outcome store from what this test measured with
// pg_stat_statements; the collector-fed verifier over full 5-minute
// windows is the lifeos run (CHECK-22).
func TestGateT5_ThreeStepPlanRevertsTheInjectedRegression(t *testing.T) {
	g := newGateRun(t)
	p := g.approvedPlan()
	stop := g.workload()
	defer stop()
	g.keepImproved(p.ID, 1, "%gate_t1 WHERE a%")
	g.killAndResumeStep2(p.ID)
	g.checkStep2RanOnce(p.ID)
	g.regressStep3(p.ID)
	stop()
	g.checkGateEnd(p.ID)
}

// keepImproved runs step ordinal after a workload window, checks its
// reads improved, records improved and ticks it to kept.
func (g *gateRun) keepImproved(id int64, ordinal int, reads string) {
	g.t.Helper()
	before, _ := g.measure(reads)
	s := g.tickUntil(g.engine, id, ordinal, StateJudging)
	after, _ := g.measure(reads)
	g.t.Logf("step %d: reads %.3f ms -> %.3f ms", ordinal, before, after)
	if !(after < before) {
		g.t.Fatalf("step %d: reads %.3f ms -> %.3f ms, want them faster", ordinal, before,
			after)
	}
	g.record(s, "improved", "reads improved")
	g.tickUntil(g.engine, id, ordinal, StateKept)
}

// killAndResumeStep2 runs step 2 to executed, drops that engine before
// its verdict, starts another once the claim expired, and checks the old
// one moves nothing.
func (g *gateRun) killAndResumeStep2(id int64) {
	g.t.Helper()
	before, _ := g.measure("%gate_t2 WHERE b%")
	old := g.engine
	s := g.tickUntil(old, id, 2, StateExecuted)
	time.Sleep(3 * time.Second) // sidecar-1 is gone; its 2s claim expires
	g.engine = g.newEngine("sidecar-2", time.Minute)
	got := g.tickUntil(g.engine, id, 2, StateJudging)
	if got.Runtime.ClaimedBy != "sidecar-2" || *got.Runtime.ActionLogID != *s.Runtime.ActionLogID {
		g.t.Fatalf("resumed step 2 %+v, want sidecar-2 judging action %d", got.Runtime,
			*s.Runtime.ActionLogID)
	}
	err := old.transition(g.ctx, id, s.ID, StateJudging, StateKept, stepWrite{claim: true})
	if !errors.Is(err, ErrStepMoved) {
		g.t.Fatalf("the old sidecar's transition: %v, want ErrStepMoved", err)
	}
	after, _ := g.measure("%gate_t2 WHERE b%")
	if !(after < before) {
		g.t.Fatalf("step 2: reads %.3f ms -> %.3f ms, want them faster", before, after)
	}
	g.record(got, "improved", "reads improved")
	g.tickUntil(g.engine, id, 2, StateKept)
}

// checkStep2RanOnce is CHECK-19: one action_log row and one CREATE INDEX
// call for t2(b).
func (g *gateRun) checkStep2RanOnce(id int64) {
	g.t.Helper()
	s := g.step(id, 2)
	var rows int
	if err := g.pool.QueryRow(g.ctx, `SELECT count(*) FROM sage.action_log
		WHERE sql_executed = $1`, s.ProposedSQL).Scan(&rows); err != nil {
		g.t.Fatalf("count step 2's ledger rows: %v", err)
	}
	calls, _, _ := g.stat("CREATE INDEX CONCURRENTLY gate_t2_b%")
	if rows != 1 || calls != 1 {
		g.t.Fatalf("CHECK-19: step 2 has %d action_log rows and %v CREATE INDEX calls, "+
			"want 1 and 1", rows, calls)
	}
	g.t.Logf("CHECK-19: step 2 has one action_log row and one CREATE INDEX call")
}

// regressStep3 runs step 3, measures its WAL per insert against the
// verifier's write_impact_pct and, past it, reverts it the verifier's way.
func (g *gateRun) regressStep3(id int64) {
	g.t.Helper()
	const reads = "%gate_t3%WHERE payload%"
	readsBefore, walBefore := g.measure(reads)
	s := g.tickUntil(g.engine, id, 3, StateJudging)
	readsAfter, walAfter := g.measure(reads)
	rise := (walAfter - walBefore) / walBefore * 100
	limit := verify.DefaultOptions().WriteImpactPct
	g.t.Logf("step 3: reads %.3f -> %.3f ms, WAL per insert %.0f -> %.0f bytes (%+.0f%%, "+
		"write_impact_pct %.0f)", readsBefore, readsAfter, walBefore, walAfter, rise, limit)
	if !(readsAfter < readsBefore) || rise <= limit {
		g.t.Fatalf("step 3 is not the injected regression: reads %.3f -> %.3f ms, WAL "+
			"%+.0f%% (limit %.0f%%)", readsBefore, readsAfter, rise, limit)
	}
	g.record(s, "regressed", "write_impact")
	if err := g.exec.RollbackAction(g.ctx, *s.Runtime.ActionLogID,
		"verifier: write_impact"); err != nil {
		g.t.Fatalf("the verifier's revert: %v", err)
	}
	got := g.tickUntil(g.engine, id, 3, StateReverted)
	if got.Runtime.StateReason != ReasonRevertedByVerifier ||
		got.Runtime.Verdict != OutcomeRegressed {
		g.t.Fatalf("step 3 %+v, want reverted_by_verifier on a regression", got.Runtime)
	}
	for range 3 {
		if err := g.engine.Tick(g.ctx); err != nil {
			g.t.Fatalf("tick: %v", err)
		}
	}
}

// checkGateEnd is CHECK-20: 1 and 2 kept, 3 reverted and its index gone,
// the plan halted, one notification, and plan_events verifies.
func (g *gateRun) checkGateEnd(id int64) {
	g.t.Helper()
	p := mustGet(g.t, g.store, id)
	got := states(p)
	if p.Status != StatusHalted || got[0] != StateKept || got[1] != StateKept ||
		got[2] != StateReverted {
		g.t.Fatalf("CHECK-20: plan %s with steps %v, want halted, kept kept reverted",
			p.Status, got)
	}
	var present bool
	if err := g.pool.QueryRow(g.ctx, `SELECT EXISTS (SELECT 1 FROM pg_index
		WHERE indexrelid = to_regclass('public.gate_t3_payload'))`).Scan(&present); err != nil {
		g.t.Fatalf("read pg_index: %v", err)
	}
	sent := g.notes.sent()
	if present || len(sent) != 1 || !strings.Contains(sent[0].Subject, "halted") {
		g.t.Fatalf("CHECK-20: step 3's index present %v, notifications %+v: want gone and "+
			"one halt", present, sent)
	}
	if rep := verifyPlanEvents(g.t, g.pool); !rep.OK() {
		g.t.Fatalf("CHECK-20: plan_events does not verify: %+v", rep)
	}
	g.t.Logf("CHECK-20: steps 1 and 2 kept, 3 reverted_by_verifier, index gone, plan " +
		"halted, one notification, plan_events verifies")
}
