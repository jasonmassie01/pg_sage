package tuner

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// tuning-06: a proposal an operator rejected becomes 'rejected', one left
// unanswered for 30 days becomes 'expired'; neither holds the statement,
// and no proposal is recorded when no hint could be installed.

// lifecycleDB is the tuner test database with the rows of ids (and their
// findings and queue items) removed before and after the test.
func lifecycleDB(t *testing.T, ids ...int64) (*pgxpool.Pool, context.Context) {
	t.Helper()
	pool := connectTunerTestDB(t)
	t.Cleanup(pool.Close)
	idents := make([]string, len(ids))
	for i, id := range ids {
		idents[i] = "queryid:" + strconv.FormatInt(id, 10)
	}
	clean := func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM sage.action_queue WHERE finding_id IN (
			SELECT id FROM sage.findings WHERE object_identifier = ANY($1))`, idents)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.findings WHERE object_identifier = ANY($1)`,
			idents)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.query_hints WHERE queryid = ANY($1)`, ids)
	}
	clean()
	t.Cleanup(clean)
	return pool, context.Background()
}

// seedDecision files a query_tuning finding for qid and an action queue
// item for it with the given status, decider and decision age.
func seedDecision(t *testing.T, pool *pgxpool.Pool, qid int64, category, status string,
	decidedBy any, decidedAgo string) {
	t.Helper()
	ctx := context.Background()
	var fid int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, status)
		VALUES ($1, 'warning', 'query', $2, 'hint', '{}'::jsonb, 'open') RETURNING id`,
		category, "queryid:"+strconv.FormatInt(qid, 10)).Scan(&fid); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_queue
		(finding_id, proposed_sql, action_risk, status, decided_by, decided_at)
		VALUES ($1, 'INSERT INTO hint_plan.hints ...', 'safe', $2, $3,
		        now() - $4::interval)`, fid, status, decidedBy, decidedAgo); err != nil {
		t.Fatalf("seed queue item: %v", err)
	}
}

func ageHint(t *testing.T, pool *pgxpool.Pool, qid int64, ago string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE sage.query_hints
		SET created_at = now() - $2::interval WHERE queryid = $1`, qid, ago); err != nil {
		t.Fatalf("age hint %d: %v", qid, err)
	}
}

// Nil/empty: without a usable hint table the finding stays advisory and no
// 'proposed' row is written (it could never be installed).
func TestHintStatus_NoHintTableRecordsNoProposal(t *testing.T) {
	const qid = int64(9_260_001)
	for name, avail := range map[string]*HintPlanAvailability{
		"nil availability": nil,
		"no hint table":    {ApplicationEffect: true},
		"no app effect":    {HintTableReady: true},
	} {
		t.Run(name, func(t *testing.T) {
			pool, ctx := lifecycleDB(t, qid)
			if avail != nil && avail.HintTableReady {
				// Claiming a ready table needs the 1.7 layout the state read uses.
				requireQueryIDHintTable(t, pool)
			}
			tu := New(pool, TunerConfig{WorkMemMaxMB: 256, MinQueryCalls: 1,
				PlanTimeRatio: 0.5}, avail, noopLog2)
			fs, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid))
			if err != nil {
				t.Fatalf("tuneCandidates: %v", err)
			}
			got := findingFor(fs, qid)
			if len(got) != 1 || got[0].RecommendedSQL != "" {
				t.Fatalf("want one advisory finding without SQL, got %+v", got)
			}
			if rows := sageHintStatuses(t, pool, qid); len(rows) != 0 {
				t.Fatalf("an uninstallable hint was recorded: %v", rows)
			}
		})
	}
}

// CHECK-08 / state transitions: proposed, then rejected by an operator,
// then the statement takes a hint again.
func TestHintStatus_RejectedProposalStopsCooling(t *testing.T) {
	const qid = int64(9_260_011)
	pool, ctx := hintScopeDB(t, qid)
	lifecycleDB(t, qid)
	recordSageHint(t, pool, qid, scopeOwnHint, "proposed")
	tu := ownershipTuner(pool)
	if _, err := tu.CheckHint(ctx, ownershipHint(qid, "SeqScan(t)")); !errors.Is(err,
		ErrHintExists) {
		t.Fatalf("an open proposal = %v, want ErrHintExists", err)
	}
	seedDecision(t, pool, qid, "query_tuning", "rejected", 1, "0 seconds")
	rejected, expired := tu.reconcileLifecycle(ctx)
	if rejected != 1 || expired != 0 {
		t.Fatalf("reconcile = %d rejected, %d expired; want 1, 0", rejected, expired)
	}
	if got := sageHintStatuses(t, pool, qid); len(got) != 1 || got[0] != "rejected" {
		t.Fatalf("statuses = %v, want [rejected]", got)
	}
	states, err := tu.hintStates(ctx, []int64{qid})
	if err != nil || states[qid] != hintNone {
		t.Fatalf("hint state = %v, %v; want none", states, err)
	}
	if _, err := tu.CheckHint(ctx, ownershipHint(qid, "SeqScan(t)")); err != nil {
		t.Fatalf("a rejected proposal still cools the statement: %v", err)
	}
}

// Invalid input: only an operator's rejection of this statement's
// query_tuning finding, decided after the proposal, rejects it.
func TestHintStatus_OtherDecisionsDoNotReject(t *testing.T) {
	cases := []struct {
		name, category, status, ago string
		by                          any
	}{
		{"system expiry", "query_tuning", "rejected", "0 seconds", nil},
		{"still pending", "query_tuning", "pending", "0 seconds", 1},
		{"approved", "query_tuning", "approved", "0 seconds", 1},
		{"other category", "plan_regression", "rejected", "0 seconds", 1},
		{"before the proposal", "query_tuning", "rejected", "2 days", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const qid = int64(9_260_021)
			pool, ctx := lifecycleDB(t, qid, qid+1)
			recordSageHint(t, pool, qid, scopeOwnHint, "proposed")
			ageHint(t, pool, qid, "1 day")
			seedDecision(t, pool, qid, tc.category, tc.status, tc.by, tc.ago)
			seedDecision(t, pool, qid+1, "query_tuning", "rejected", 1, "0 seconds")
			tu := New(pool, TunerConfig{}, nil, noopLog2)
			if rejected, _ := tu.reconcileLifecycle(ctx); rejected != 0 {
				t.Fatalf("%d proposals rejected, want 0", rejected)
			}
			if got := sageHintStatuses(t, pool, qid); len(got) != 1 || got[0] != "proposed" {
				t.Fatalf("statuses = %v, want [proposed]", got)
			}
		})
	}
}

// CHECK-10 / boundary: a proposal older than 30 days expires in one cycle;
// a 29-day-old one and an old active hint do not.
func TestHintStatus_StaleProposalExpires(t *testing.T) {
	const old, young, live = int64(9_260_031), int64(9_260_032), int64(9_260_033)
	pool, ctx := lifecycleDB(t, old, young, live)
	recordSageHint(t, pool, old, scopeOwnHint, "proposed")
	recordSageHint(t, pool, young, scopeOwnHint, "proposed")
	recordSageHint(t, pool, live, scopeOwnHint, "active")
	// Hours, not days: a calendar day across a DST change is 23 or 25 hours
	// in a non-UTC session, and proposalExpiry is an absolute 720 hours.
	ageHint(t, pool, old, "720 hours 1 minute")
	ageHint(t, pool, young, "719 hours")
	ageHint(t, pool, live, "90 days")
	tu := New(pool, TunerConfig{}, nil, noopLog2)
	if _, expired := tu.reconcileLifecycle(ctx); expired != 1 {
		t.Fatalf("%d proposals expired, want 1", expired)
	}
	for qid, want := range map[int64]string{old: "expired", young: "proposed",
		live: "active"} {
		if got := sageHintStatuses(t, pool, qid); len(got) != 1 || got[0] != want {
			t.Fatalf("queryid %d statuses = %v, want [%s]", qid, got, want)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.query_hints
		WHERE status = 'proposed' AND created_at < now() - interval '30 days'`).
		Scan(&n); err != nil || n != 0 {
		t.Fatalf("CHECK-10: %d stale proposals left (%v), want 0", n, err)
	}
}

// State transitions: Tune runs the lifecycle every cycle, and an expired
// statement gets a fresh proposal row rather than reviving the old one.
func TestHintStatus_TuneExpiresAndReproposes(t *testing.T) {
	const qid = int64(9_260_041)
	pool, ctx := hintScopeDB(t, qid)
	lifecycleDB(t, qid)
	recordSageHint(t, pool, qid, "Set(plan_cache_mode force_generic_plan)", "proposed")
	ageHint(t, pool, qid, "31 days")
	tu := ownershipTuner(pool)
	tu.reconcileHintStatuses(ctx)
	fs, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid))
	if err != nil {
		t.Fatalf("tuneCandidates: %v", err)
	}
	if len(findingFor(fs, qid)) != 1 {
		t.Fatalf("an expired proposal still holds the statement: %+v", fs)
	}
	if got := sageHintStatuses(t, pool, qid); len(got) != 2 || got[0] != "expired" ||
		got[1] != "proposed" {
		t.Fatalf("statuses = %v, want [expired proposed]", got)
	}
}

// The deterministic pass does not propose again, within 30 days, the hint
// an operator rejected for the statement; an older rejection does not bind.
func TestHintStatus_RejectedHintIsNotReproposed(t *testing.T) {
	const qid = int64(9_260_051)
	pool, ctx := hintScopeDB(t, qid)
	lifecycleDB(t, qid)
	tu := ownershipTuner(pool)
	if fs, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid)); err != nil ||
		len(findingFor(fs, qid)) != 1 {
		t.Fatalf("first proposal: %+v, %v", fs, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.query_hints SET status = 'rejected'
		WHERE queryid = $1`, qid); err != nil {
		t.Fatal(err)
	}
	tu.recentlyTuned = map[int64]int{}
	fs, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid))
	if err != nil || len(findingFor(fs, qid)) != 0 {
		t.Fatalf("the rejected hint was proposed again: %+v, %v", fs, err)
	}
	if got := sageHintStatuses(t, pool, qid); len(got) != 1 || got[0] != "rejected" {
		t.Fatalf("statuses = %v, want only [rejected]", got)
	}
	ageHint(t, pool, qid, "31 days")
	tu.recentlyTuned = map[int64]int{}
	if fs, err := tuneLocked(ctx, tu, spillHintCandidate(tu, qid)); err != nil ||
		len(findingFor(fs, qid)) != 1 {
		t.Fatalf("a rejection older than 30 days still binds: %+v, %v", fs, err)
	}
}

// Concurrent access: two tuners reconciling the same rejected and stale
// proposals at once make exactly one transition each.
func TestHintStatus_ConcurrentReconcileOneTransition(t *testing.T) {
	const rej, stale = int64(9_260_061), int64(9_260_062)
	pool, ctx := lifecycleDB(t, rej, stale)
	recordSageHint(t, pool, rej, scopeOwnHint, "proposed")
	recordSageHint(t, pool, stale, scopeOwnHint, "proposed")
	ageHint(t, pool, stale, "40 days")
	seedDecision(t, pool, rej, "query_tuning", "rejected", 1, "0 seconds")
	var mu sync.Mutex
	var rejected, expired int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := New(pool, TunerConfig{}, nil, noopLog2).reconcileLifecycle(ctx)
			mu.Lock()
			rejected, expired = rejected+r, expired+e
			mu.Unlock()
		}()
	}
	wg.Wait()
	if rejected != 1 || expired != 1 {
		t.Fatalf("transitions = %d rejected, %d expired; want exactly 1 each",
			rejected, expired)
	}
}

// Error propagation: a failed lifecycle step is logged with what it tried
// and changes nothing.
func TestHintStatus_ReconcileErrorIsLogged(t *testing.T) {
	pool := connectTunerTestDB(t)
	pool.Close()
	var logs []string
	tu := New(pool, TunerConfig{}, nil, func(_ string, f string, args ...any) {
		logs = append(logs, fmt.Sprintf(f, args...))
	})
	if r, e := tu.reconcileLifecycle(context.Background()); r != 0 || e != 0 {
		t.Fatalf("a closed pool reported %d, %d transitions", r, e)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "rejected") || !strings.Contains(joined, "expired") {
		t.Fatalf("the failed steps are not logged: %q", joined)
	}
	if r, e := New(nil, TunerConfig{}, nil, noopLog2).reconcileLifecycle(
		context.Background()); r != 0 || e != 0 {
		t.Fatal("no pool reported transitions")
	}
}
