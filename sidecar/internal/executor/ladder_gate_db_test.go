package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/verify"
)

type ladderFixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	exec *Executor
}

func newLadderFixture(t *testing.T) *ladderFixture {
	t.Helper()
	pool, ctx := requireDB(t)
	exec := New(pool, autonomousTestConfig(), time.Now().Add(-90*24*time.Hour), noopExecLog)
	exec.WithActionStore(store.NewActionStore(pool), "auto")
	return &ladderFixture{t: t, ctx: ctx, pool: pool, exec: exec}
}

// finding inserts an open finding with detail and returns its id.
func (f *ladderFixture) finding(af analyzer.Finding) int {
	f.t.Helper()
	detail, err := json.Marshal(af.Detail)
	if err != nil {
		f.t.Fatal(err)
	}
	var id int
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommended_sql, rollback_sql,
		status) VALUES ($1, 'warning', 'index', $2, 'ladder test', $3::jsonb, $4, $5, 'open')
		RETURNING id`, af.Category, af.ObjectIdentifier, detail, af.RecommendedSQL,
		af.RollbackSQL).Scan(&id); err != nil {
		f.t.Fatalf("insert finding: %v", err)
	}
	f.t.Cleanup(func() {
		bg := context.Background()
		_, _ = f.pool.Exec(bg, "DELETE FROM sage.action_queue WHERE finding_id = $1", id)
		_, _ = f.pool.Exec(bg, "DELETE FROM sage.findings WHERE id = $1", id)
	})
	return id
}

func (f *ladderFixture) queueLadder(queueID int) []verify.LadderRecord {
	f.t.Helper()
	raw, err := verify.ReadLadder(f.ctx, f.pool, queueID)
	if err != nil {
		f.t.Fatalf("read ladder: %v", err)
	}
	recs, err := verify.ParseLadder(raw)
	if err != nil {
		f.t.Fatalf("parse ladder %s: %v", raw, err)
	}
	return recs
}

func (f *ladderFixture) actionLog(sql string) int64 {
	f.t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.action_log (action_type,
		sql_executed, outcome) VALUES ('create_index', $1, 'monitoring') RETURNING id`,
		sql).Scan(&id); err != nil {
		f.t.Fatalf("insert action: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id = $1",
			id)
	})
	return id
}

// Happy path: an index create with a HypoPG what-if is queued with a
// tier-1 plan_only record and its predicted change; at execution the
// outcome row's prediction carries the same record.
func TestLadderDB_WhatIfReachesQueueAndOutcome(t *testing.T) {
	f := newLadderFixture(t)
	n := time.Now().UnixNano()
	af := optimizerFinding("verified")
	af.ObjectIdentifier = fmt.Sprintf("public.orders|btree(status_%d)", n)
	af.RecommendedSQL = fmt.Sprintf(
		"CREATE INDEX CONCURRENTLY idx_ladder_%d ON public.orders (status)", n)
	af.Detail["estimated_improvement_pct"] = 41.0
	af.Detail["what_if_measured"] = 4
	findingID := f.finding(af)
	queueID, err := f.exec.proposeForApproval(f.ctx, findingID, af, nil)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	queued := f.queueLadder(queueID)
	if len(queued) != 1 || queued[0].Tier != verify.TierPlanOnly ||
		queued[0].Name != "plan_only" || !queued[0].Passed() {
		t.Fatalf("queue ladder = %+v, want one tier-1 plan_only pass", queued)
	}
	if queued[0].Predicted == nil || *queued[0].Predicted != -41 {
		t.Fatalf("queue predicted = %v, want -41", queued[0].Predicted)
	}

	ctx := withQueueLadder(f.ctx, queued)
	before := map[string]any{}
	f.exec.predictAction(ctx, af.RecommendedSQL, af.Detail, before)
	actionID := f.actionLog(af.RecommendedSQL)
	f.exec.recordPrediction(ctx, actionID, before)
	var raw []byte
	if err := f.pool.QueryRow(f.ctx, `SELECT predicted->'ladder' FROM sage.action_outcome
		WHERE action_log_id = $1`, actionID).Scan(&raw); err != nil {
		t.Fatalf("read outcome ladder: %v", err)
	}
	got, err := verify.ParseLadder(raw)
	if err != nil || len(got) != 1 {
		t.Fatalf("outcome ladder = %s (%v), want one record", raw, err)
	}
	if got[0].Tier != queued[0].Tier || got[0].Method != queued[0].Method ||
		*got[0].Predicted != *queued[0].Predicted || !got[0].At.Equal(queued[0].At) {
		t.Fatalf("outcome record = %+v, want the queued %+v", got[0], queued[0])
	}
}

// The verdict appends tier 3 to the queue row that ran the action.
func TestLadderDB_VerdictAppendsTierThree(t *testing.T) {
	f := newLadderFixture(t)
	n := time.Now().UnixNano()
	af := ladderDropFinding(map[string]any{"ladder": ladderDetail(t, passedClone())})
	af.ObjectIdentifier = fmt.Sprintf("public.idx_ladder_%d", n)
	af.RecommendedSQL = fmt.Sprintf(`DROP INDEX CONCURRENTLY "public"."idx_ladder_%d"`, n)
	findingID := f.finding(af)
	queueID, err := f.exec.proposeForApproval(f.ctx, findingID, af, nil)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	actionID := f.actionLog(af.RecommendedSQL)
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_queue SET action_log_id = $1
		WHERE id = $2`, actionID, queueID); err != nil {
		t.Fatal(err)
	}
	change := -12.0
	appendVerdictTier(f.ctx, f.pool, verify.Outcome{ActionLogID: actionID,
		Verdict: verify.OutcomeImproved, Observed: verify.Observed{ChangePct: &change}},
		noopExecLog)
	recs := f.queueLadder(queueID)
	if len(recs) < 3 {
		t.Fatalf("ladder = %+v, want the drop what-if or skip, the clone, then tier 3", recs)
	}
	last := recs[len(recs)-1]
	if last.Tier != verify.TierCanary || !last.Passed() || *last.Predicted != -12 {
		t.Fatalf("last record = %+v, want a passed tier 3 with -12", last)
	}
	if recs[len(recs)-2].Tier != verify.TierClone {
		t.Fatalf("records = %+v, want the clone record before tier 3", recs)
	}
}

// The approved path reads the queue item's ladder into the context; an
// unreadable or missing one is logged and the finding's ladder applies.
func TestLadderDB_QueueLadderReachesTheContext(t *testing.T) {
	f := newLadderFixture(t)
	var logged []string
	f.exec.logFn = func(_ string, format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	n := time.Now().UnixNano()
	af := ladderDropFinding(map[string]any{"ladder": ladderDetail(t, passedClone())})
	af.ObjectIdentifier = fmt.Sprintf("public.idx_ctx_%d", n)
	af.RecommendedSQL = fmt.Sprintf(`DROP INDEX CONCURRENTLY "public"."idx_ctx_%d"`, n)
	queueID, err := f.exec.proposeForApproval(f.ctx, f.finding(af), af, nil)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	recs := queueLadderFrom(f.exec.withQueueLadderOf(f.ctx, queueID))
	if len(recs) != 2 || recs[1].Tier != verify.TierClone || !recs[1].Passed() {
		t.Fatalf("context ladder = %+v, want the skipped what-if and the clone", recs)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_queue SET ladder = '{"x":1}'
		WHERE id = $1`, queueID); err != nil {
		t.Fatal(err)
	}
	if got := queueLadderFrom(f.exec.withQueueLadderOf(f.ctx, queueID)); got != nil {
		t.Fatalf("unreadable ladder reached the context: %+v", got)
	}
	if got := queueLadderFrom(f.exec.withQueueLadderOf(f.ctx, -1)); got != nil {
		t.Fatalf("a missing queue item gave a ladder: %+v", got)
	}
	if len(logged) != 2 || !strings.Contains(logged[0], fmt.Sprintf("queue item %d", queueID)) {
		t.Fatalf("logged = %q, want two lines naming the queue item", logged)
	}
}

// A verdict recorded again (a retried revert, a re-verification) replaces
// the action's tier-3 record instead of stacking another; the records
// before it stay in order.
func TestLadderDB_ReRecordedVerdictReplacesTierThree(t *testing.T) {
	f := newLadderFixture(t)
	n := time.Now().UnixNano()
	af := ladderDropFinding(map[string]any{"ladder": ladderDetail(t, passedClone())})
	af.ObjectIdentifier = fmt.Sprintf("public.idx_rerecord_%d", n)
	af.RecommendedSQL = fmt.Sprintf(`DROP INDEX CONCURRENTLY "public"."idx_rerecord_%d"`, n)
	queueID, err := f.exec.proposeForApproval(f.ctx, f.finding(af), af, nil)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	before := f.queueLadder(queueID)
	actionID := f.actionLog(af.RecommendedSQL)
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_queue SET action_log_id = $1
		WHERE id = $2`, actionID, queueID); err != nil {
		t.Fatal(err)
	}
	regressed, improved := 9.0, -4.0
	for _, o := range []verify.Outcome{
		{ActionLogID: actionID, Verdict: verify.OutcomeRegressed,
			Observed: verify.Observed{ChangePct: &regressed}},
		{ActionLogID: actionID, Verdict: verify.OutcomeRegressed,
			Observed: verify.Observed{ChangePct: &regressed}},
		{ActionLogID: actionID, Verdict: verify.OutcomeImproved,
			Observed: verify.Observed{ChangePct: &improved}},
	} {
		appendVerdictTier(f.ctx, f.pool, o, noopExecLog)
	}
	recs := f.queueLadder(queueID)
	if len(recs) != len(before)+1 {
		t.Fatalf("ladder = %+v, want the %d earlier records and one tier 3", recs, len(before))
	}
	for i := range before {
		if recs[i].Tier != before[i].Tier || recs[i].Result != before[i].Result {
			t.Fatalf("record %d = %+v, want the earlier %+v", i, recs[i], before[i])
		}
	}
	last := recs[len(recs)-1]
	if last.Tier != verify.TierCanary || !last.Passed() || *last.Predicted != -4 ||
		last.Evidence["verdict"] != verify.OutcomeImproved {
		t.Fatalf("tier 3 = %+v, want the latest verdict, improved -4", last)
	}
}
