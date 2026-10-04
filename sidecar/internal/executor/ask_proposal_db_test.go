package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// Ask Sage (roadmap phase 3) may queue one of pg_sage's own findings for
// a person's approval, and nothing more: ProposeFindingForApproval asks
// the standing gate (Explain: no decision recorded, no approval flags),
// refuses what the gate blocks or what has no typed contract or rollback,
// queues the finding's own SQL as a pending approval item with its
// predicted effect, and never executes, even when the gate would.

type recordingExplainer struct {
	mu       sync.Mutex
	inner    policy.Gate
	requests []policy.ActionRequest
}

func (g *recordingExplainer) Authorize(ctx context.Context,
	r policy.ActionRequest) policy.Decision {
	g.mu.Lock()
	g.requests = append(g.requests, r)
	g.mu.Unlock()
	return g.inner.Authorize(ctx, r)
}

func (g *recordingExplainer) Explain(ctx context.Context,
	r policy.ActionRequest) policy.Decision {
	g.mu.Lock()
	g.requests = append(g.requests, r)
	g.mu.Unlock()
	return g.inner.(policy.Explainer).Explain(ctx, r)
}

type askProposalFixture struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	exec     *Executor
	gate     *recordingExplainer
	recorded int
}

func newAskProposalFixture(t *testing.T) *askProposalFixture {
	t.Helper()
	pool, ctx := requireDB(t)
	f := &askProposalFixture{t: t, ctx: ctx, pool: pool}
	f.sql(`DROP TABLE IF EXISTS public.ask_px`)
	f.sql(`CREATE TABLE public.ask_px (id int, c int)`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS public.ask_px`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.action_queue
			WHERE proposed_sql LIKE '%ask_px%'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.findings
			WHERE object_identifier LIKE 'public.ask_px%'`)
	})
	now := time.Now()
	f.gate = &recordingExplainer{inner: explainTestGate(now, &f.recorded)}
	f.exec = New(pool, autonomousTestConfig(), now.Add(-90*24*time.Hour), noopExecLog)
	f.exec.WithPolicyGate(f.gate)
	f.exec.WithActionStore(store.NewActionStore(pool), "auto")
	return f
}

func (f *askProposalFixture) sql(q string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, q, args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func (f *askProposalFixture) finding(object, sql, rollback, status string) int64 {
	f.t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommended_sql, rollback_sql, status)
		VALUES ('missing_index', 'warning', 'table', $1, 'ask proposal test', '{}'::jsonb,
		NULLIF($2, ''), NULLIF($3, ''), $4) RETURNING id`, object, sql, rollback,
		status).Scan(&id); err != nil {
		f.t.Fatalf("seed finding: %v", err)
	}
	return id
}

func (f *askProposalFixture) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, q, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	return n
}

const askPxCreate = "CREATE INDEX CONCURRENTLY ask_px_c ON public.ask_px (c)"

func TestProposeFindingForApproval_QueuesAndNeverExecutes(t *testing.T) {
	f := newAskProposalFixture(t)
	id := f.finding("public.ask_px", askPxCreate, "DROP INDEX CONCURRENTLY public.ask_px_c",
		"open")
	p, err := f.exec.ProposeFindingForApproval(f.ctx, id)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if !p.Created || p.QueueID <= 0 || p.FindingID != id || p.SQL != askPxCreate ||
		p.RollbackSQL != "DROP INDEX CONCURRENTLY public.ask_px_c" ||
		p.RollbackClass != "reversible" || p.ActionType != "create_index_concurrently" {
		t.Fatalf("proposal = %+v", p)
	}
	// The gate would execute this on its own initiative; Ask Sage still
	// only queues it for a person.
	if p.Decision.Decision != PolicyDecisionExecute {
		t.Fatalf("gate decision = %+v, want the autonomous gate's execute", p.Decision)
	}
	if p.Prediction.Class == "" {
		t.Fatalf("no predicted effect: %+v", p.Prediction)
	}
	if n := f.count(`SELECT count(*) FROM sage.action_queue WHERE id = $1 AND status =
		'pending' AND finding_id = $2 AND rollback_sql IS NOT NULL AND decided_by IS NULL`,
		p.QueueID, id); n != 1 {
		t.Fatalf("queue item %d not pending with its rollback (%d rows)", p.QueueID, n)
	}
	if n := f.count(`SELECT count(*) FROM pg_class WHERE relname = 'ask_px_c'`); n != 0 {
		t.Fatal("the index was created: a proposal executed")
	}
	if n := f.count(`SELECT count(*) FROM sage.action_log WHERE finding_id = $1`, id); n != 0 {
		t.Fatalf("%d actions logged for a proposal", n)
	}
	if f.recorded != 0 {
		t.Fatalf("the gate recorded %d decisions; a proposal uses Explain", f.recorded)
	}
	for _, r := range f.gate.requests {
		if r.OperatorApproved || r.OwnerDeclared || r.Rollback || r.LeaseHeld {
			t.Fatalf("a proposal asked the gate with approval flags: %+v", r)
		}
	}
	again, err := f.exec.ProposeFindingForApproval(f.ctx, id)
	if err != nil || again.Created || again.QueueID != p.QueueID {
		t.Fatalf("second proposal = %+v (%v), want the pending item %d", again, err, p.QueueID)
	}
	if n := f.count(`SELECT count(*) FROM sage.action_queue WHERE finding_id = $1`, id); n != 1 {
		t.Fatalf("%d queue items for one finding", n)
	}
}

func TestProposeFindingForApproval_NoRollbackNeededIsAllowed(t *testing.T) {
	f := newAskProposalFixture(t)
	id := f.finding("public.ask_px", "ANALYZE public.ask_px", "", "open")
	p, err := f.exec.ProposeFindingForApproval(f.ctx, id)
	if err != nil || !p.Created || p.RollbackClass != "no_rollback_needed" {
		t.Fatalf("analyze proposal = %+v (%v)", p, err)
	}
}

func TestProposeFindingForApproval_Refusals(t *testing.T) {
	f := newAskProposalFixture(t)
	cases := map[string]int64{
		"resolved":     f.finding("public.ask_px", askPxCreate, "DROP INDEX x", "resolved"),
		"no sql":       f.finding("public.ask_px", "", "", "open"),
		"untyped":      f.finding("public.ask_px_t", "DROP TABLE public.ask_px", "", "open"),
		"no rollback":  f.finding("public.ask_px_r", askPxCreate, "", "open"),
		"missing":      999_999_999,
		"zero id":      0,
		"negative id":  -4,
		"truncate sql": f.finding("public.ask_px_u", "TRUNCATE public.ask_px", "", "open"),
	}
	for name, id := range cases {
		_, err := f.exec.ProposeFindingForApproval(f.ctx, id)
		if !errors.Is(err, ErrNotProposable) {
			t.Errorf("%s: err = %v, want ErrNotProposable", name, err)
		}
	}
	if n := f.count(`SELECT count(*) FROM sage.action_queue
		WHERE proposed_sql LIKE '%ask_px%'`); n != 0 {
		t.Fatalf("refused proposals queued %d items", n)
	}
	if n := f.count(`SELECT count(*) FROM pg_class WHERE relname = 'ask_px'`); n != 1 {
		t.Fatal("the table was dropped")
	}
}

func TestProposeFindingForApproval_GateBlockQueuesNothing(t *testing.T) {
	f := newAskProposalFixture(t)
	id := f.finding("public.ask_px", askPxCreate, "DROP INDEX CONCURRENTLY public.ask_px_c",
		"open")
	f.exec.WithPolicyGate(nil) // no standing gate: fail closed
	p, err := f.exec.ProposeFindingForApproval(f.ctx, id)
	if !errors.Is(err, ErrProposalBlocked) || p.Decision.Decision != PolicyDecisionBlocked {
		t.Fatalf("blocked proposal = %+v (%v)", p, err)
	}
	if !strings.Contains(err.Error(), reasonNoStandingPolicy) {
		t.Fatalf("the error does not name the reason: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM sage.action_queue WHERE finding_id = $1`, id); n != 0 {
		t.Fatalf("a blocked proposal queued %d items", n)
	}
}

func TestProposeFindingForApproval_NoQueueOrExecutor(t *testing.T) {
	f := newAskProposalFixture(t)
	id := f.finding("public.ask_px", askPxCreate, "DROP INDEX CONCURRENTLY public.ask_px_c",
		"open")
	bare := New(f.pool, autonomousTestConfig(), time.Now().Add(-90*24*time.Hour), noopExecLog)
	bare.WithPolicyGate(f.gate)
	if _, err := bare.ProposeFindingForApproval(f.ctx, id); !errors.Is(err,
		ErrApprovalQueueUnavailable) {
		t.Fatalf("no action store: err = %v", err)
	}
	var nilExec *Executor
	if _, err := nilExec.ProposeFindingForApproval(f.ctx, id); !errors.Is(err,
		ErrApprovalQueueUnavailable) {
		t.Fatalf("nil executor: err = %v", err)
	}
}

func TestProposeFindingForApproval_ConcurrentCallsQueueOnce(t *testing.T) {
	f := newAskProposalFixture(t)
	id := f.finding("public.ask_px", askPxCreate, "DROP INDEX CONCURRENTLY public.ask_px_c",
		"open")
	var wg sync.WaitGroup
	created := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := f.exec.ProposeFindingForApproval(f.ctx, id)
			if err != nil {
				t.Errorf("propose: %v", err)
				return
			}
			created <- p.Created
		}()
	}
	wg.Wait()
	close(created)
	n := 0
	for c := range created {
		if c {
			n++
		}
	}
	if n != 1 || f.count(`SELECT count(*) FROM sage.action_queue WHERE finding_id = $1`,
		id) != 1 {
		t.Fatalf("%d proposals created; want exactly one queue item", n)
	}
}

// fixedGate answers every request with one decision.
type fixedGate struct{ d policy.Decision }

func (g fixedGate) Authorize(context.Context, policy.ActionRequest) policy.Decision { return g.d }
func (g fixedGate) Explain(context.Context, policy.ActionRequest) policy.Decision   { return g.d }

// A self-initiated block that a person's approval lifts (the trust ramp,
// approval required, budgets, windows, earned autonomy) is still queued
// for that person; a block an approval cannot lift (emergency stop, a
// replica, a binding fact, observe-only trust) is refused.
func TestProposeFindingForApproval_QueuesWhatAnApprovalCanLift(t *testing.T) {
	f := newAskProposalFixture(t)
	liftable := []policy.Reason{policy.ReasonTrustRampNotSatisfied,
		policy.ReasonApprovalRequired, policy.ReasonBudgetExceeded,
		policy.ReasonOutsideMaintenanceWindow, policy.ReasonAutonomyLevel}
	for i, reason := range liftable {
		id := f.finding(fmt.Sprintf("public.ask_px_l%d", i), askPxCreate,
			"DROP INDEX CONCURRENTLY public.ask_px_c", "open")
		f.exec.WithPolicyGate(fixedGate{policy.Decision{Verdict: policy.VerdictBlocked,
			Reason: reason, RiskTier: policy.RiskModerate}})
		p, err := f.exec.ProposeFindingForApproval(f.ctx, id)
		if err != nil || !p.Created || p.Decision.BlockedReason != string(reason) {
			t.Errorf("%s: proposal = %+v (%v), want queued with the reason", reason, p, err)
		}
	}
	hard := []policy.Decision{
		{Verdict: policy.VerdictBlocked, Reason: policy.ReasonEmergencyStop},
		{Verdict: policy.VerdictBlocked, Reason: policy.ReasonReplicaMutation},
		{Verdict: policy.VerdictBlocked, Reason: policy.ReasonBoundByFact},
		{Verdict: policy.VerdictBlocked, Reason: policy.ReasonChangeClassNotAllowed},
		{Verdict: policy.VerdictBlocked, Reason: "some_future_reason"},
		{Verdict: policy.VerdictObserveOnly, Reason: policy.ReasonObserveOnly},
	}
	for i, d := range hard {
		id := f.finding(fmt.Sprintf("public.ask_px_h%d", i), askPxCreate,
			"DROP INDEX CONCURRENTLY public.ask_px_c", "open")
		f.exec.WithPolicyGate(fixedGate{d})
		if _, err := f.exec.ProposeFindingForApproval(f.ctx, id); !errors.Is(err,
			ErrProposalBlocked) {
			t.Errorf("%s: err = %v, want ErrProposalBlocked", d.Reason, err)
		}
		if n := f.count(`SELECT count(*) FROM sage.action_queue WHERE finding_id = $1`,
			id); n != 0 {
			t.Errorf("%s queued %d items", d.Reason, n)
		}
	}
}
