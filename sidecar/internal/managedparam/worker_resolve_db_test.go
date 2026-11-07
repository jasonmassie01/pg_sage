package managedparam

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var errThrottled = errors.New("rds DescribeDBInstances: Throttling: rate exceeded")

func (f *fakeResolver) set(target Target, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.target, f.err = target, err
}

// newFastWorker re-resolves the target on every cycle, so a resolver whose
// answer changes between cycles is seen at once.
func newFastWorker(t *testing.T, pool *pgxpool.Pool, r Resolver) *Worker {
	t.Helper()
	w, err := NewWorker(WorkerOptions{Database: "orders", Provider: "rds", Pool: pool,
		Store: NewStore(pool), Resolver: r, TargetTTL: time.Nanosecond})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w
}

func mustCycle(t *testing.T, w *Worker) CycleResult {
	t.Helper()
	res, err := w.Cycle(context.Background())
	if err != nil {
		t.Fatalf("Cycle: %v", err)
	}
	return res
}

func onlyRecord(t *testing.T, s *Store) Record {
	t.Helper()
	recs, err := s.List(context.Background(), nil, 50)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %+v, %v; want exactly one", recs, err)
	}
	return recs[0]
}

func storedTarget(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var target string
	if err := pool.QueryRow(context.Background(), `SELECT target
		FROM sage.managed_change_proposals WHERE id = $1`, id).Scan(&target); err != nil {
		t.Fatal(err)
	}
	return target
}

// State transition: resolved, approved, a resolver outage (on the same
// worker and on a restarted one), resolved again. The operator's approval
// survives and the stored command keeps the real parameter group.
func TestWorkerApprovalSurvivesResolverOutage(t *testing.T) {
	pool, ctx := testPool(t)
	r := &fakeResolver{target: rdsTarget()}
	w := newFastWorker(t, pool, r)
	insertIntentFinding(t, pool, "instance:work_mem", intent(t, "rds", "work_mem", "96MB"))
	if res := mustCycle(t, w); res.Proposed != 1 {
		t.Fatalf("first cycle = %+v", res)
	}
	first := onlyRecord(t, w.store)
	if _, err := w.store.Decide(ctx, first.ID, true, 7, "apply in the window"); err != nil {
		t.Fatal(err)
	}
	r.set(Target{}, errThrottled)
	res := mustCycle(t, w)
	if res.TargetError == "" || res.Superseded != 0 || res.Proposed != 0 {
		t.Fatalf("outage cycle = %+v, want the error reported and nothing superseded", res)
	}
	during := onlyRecord(t, w.store)
	if !strings.Contains(during.Proposal.CLI, "orders-pg16") ||
		!notesMention(during.Proposal.Notes, "Throttling") {
		t.Fatalf("an outage builds on the last resolved target with a note: %+v",
			during.Proposal)
	}
	restarted := newFastWorker(t, pool, &fakeResolver{err: errThrottled})
	if res := mustCycle(t, restarted); res.Superseded != 0 || res.Proposed != 0 {
		t.Fatalf("restarted outage cycle = %+v", res)
	}
	kept := onlyRecord(t, w.store)
	if !strings.Contains(kept.Proposal.CLI, "orders-pg16") ||
		storedTarget(t, pool, kept.ID) != "orders-pg16" {
		t.Fatalf("a placeholder build overwrote the resolved proposal: %+v", kept.Proposal)
	}
	r.set(rdsTarget(), nil)
	mustCycle(t, w)
	final := onlyRecord(t, w.store)
	if final.ID != first.ID || final.Status != StatusApproved || final.DecidedBy == nil ||
		*final.DecidedBy != 7 || final.DecisionNote != "apply in the window" {
		t.Fatalf("after the outage = %+v, want record %d still approved by 7", final, first.ID)
	}
	if notesMention(final.Proposal.Notes, "Throttling") {
		t.Fatalf("a good cycle drops the outage note: %v", final.Proposal.Notes)
	}
}

func notesMention(notes []string, s string) bool {
	for _, n := range notes {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// Rejection cooldown: a resolver outage does not bring a rejected change
// back, neither on the same worker nor after a restart.
func TestWorkerRejectionCooldownSurvivesResolverOutage(t *testing.T) {
	pool, ctx := testPool(t)
	r := &fakeResolver{target: rdsTarget()}
	w := newFastWorker(t, pool, r)
	insertIntentFinding(t, pool, "instance:work_mem", intent(t, "rds", "work_mem", "96MB"))
	mustCycle(t, w)
	rec := onlyRecord(t, w.store)
	if _, err := w.store.Decide(ctx, rec.ID, false, 3, "not on this instance"); err != nil {
		t.Fatal(err)
	}
	r.set(Target{}, errThrottled)
	for _, wk := range []*Worker{w, newFastWorker(t, pool, &fakeResolver{err: errThrottled})} {
		if res := mustCycle(t, wk); res.Proposed != 0 {
			t.Fatalf("outage cycle re-proposed a rejected change: %+v", res)
		}
	}
	got := onlyRecord(t, w.store)
	if got.ID != rec.ID || got.Status != StatusRejected {
		t.Fatalf("records = %+v, want only the rejection", got)
	}
}

// Error propagation: a first cycle that cannot resolve stores one
// placeholder proposal; the first good cycle refreshes that same record
// with the real command and target.
func TestWorkerPlaceholderRefreshedOnResolve(t *testing.T) {
	pool, _ := testPool(t)
	r := &fakeResolver{err: errThrottled}
	w := newFastWorker(t, pool, r)
	insertIntentFinding(t, pool, "instance:work_mem", intent(t, "rds", "work_mem", "96MB"))
	res := mustCycle(t, w)
	if res.Proposed != 1 || !strings.Contains(res.TargetError, "Throttling") {
		t.Fatalf("failing first cycle = %+v", res)
	}
	placeholder := onlyRecord(t, w.store)
	if !strings.Contains(placeholder.Proposal.CLI, "<db-parameter-group>") {
		t.Fatalf("with nothing resolved the command carries placeholders: %q",
			placeholder.Proposal.CLI)
	}
	r.set(rdsTarget(), nil)
	res = mustCycle(t, w)
	if res.Proposed != 0 || res.Refreshed != 1 || res.Superseded != 0 {
		t.Fatalf("resolving cycle = %+v, want the record refreshed", res)
	}
	real := onlyRecord(t, w.store)
	if real.ID != placeholder.ID || real.Status != StatusPending ||
		!strings.Contains(real.Proposal.CLI, "--db-parameter-group-name orders-pg16") ||
		storedTarget(t, pool, real.ID) != "orders-pg16" {
		t.Fatalf("refreshed = %+v (target %q), want record %d with the real command",
			real, storedTarget(t, pool, real.ID), placeholder.ID)
	}
}

// flapResolver fails every other call.
type flapResolver struct {
	mu    sync.Mutex
	calls int
}

func (f *flapResolver) Target(context.Context) (Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls%2 == 0 {
		return Target{}, fmt.Errorf("flap %d: %w", f.calls, errThrottled)
	}
	return rdsTarget(), nil
}

// Concurrency: two workers on one database with flapping resolvers end
// with one open proposal per parameter and nothing superseded.
func TestWorkersWithFlappingResolverKeepOneOpenProposal(t *testing.T) {
	pool, ctx := testPool(t)
	insertIntentFinding(t, pool, "instance:work_mem", intent(t, "rds", "work_mem", "96MB"))
	insertIntentFinding(t, pool, "instance:maintenance_work_mem",
		intent(t, "rds", "maintenance_work_mem", "333MB"))
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 2; i++ {
		w := newFastWorker(t, pool, &flapResolver{calls: i})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := 0; c < 20; c++ {
				if _, err := w.Cycle(ctx); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("cycle: %v", err)
	}
	var open, superseded, total int
	if err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status IN ('pending', 'approved')),
		count(*) FILTER (WHERE status = 'superseded'), count(*)
		FROM sage.managed_change_proposals`).Scan(&open, &superseded, &total); err != nil {
		t.Fatal(err)
	}
	if open != 2 || superseded != 0 || total != 2 {
		t.Fatalf("open=%d superseded=%d total=%d, want 2, 0, 2", open, superseded, total)
	}
}

// State transition: a finding that goes away during a resolver outage
// keeps its proposal until a cycle resolves the target again; a failed
// cycle supersedes nothing and reports no drift from the stale target.
func TestWorkerOutageDefersSupersede(t *testing.T) {
	pool, ctx := testPool(t)
	r := &fakeResolver{target: rdsTarget()}
	w := newFastWorker(t, pool, r)
	id := insertIntentFinding(t, pool, "instance:work_mem", intent(t, "rds", "work_mem", "96MB"))
	mustCycle(t, w)
	rec := onlyRecord(t, w.store)
	if _, err := pool.Exec(ctx, `UPDATE sage.findings SET status = 'resolved'
		WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	r.set(Target{}, errThrottled)
	if res := mustCycle(t, w); res.Superseded != 0 || res.Drift != nil {
		t.Fatalf("outage cycle = %+v, want nothing superseded and no drift", res)
	}
	if got := onlyRecord(t, w.store); got.ID != rec.ID || got.Status != StatusPending {
		t.Fatalf("during the outage = %+v, want record %d still pending", got, rec.ID)
	}
	r.set(rdsTarget(), nil)
	if res := mustCycle(t, w); res.Superseded != 1 {
		t.Fatalf("resolving cycle = %+v, want the orphaned proposal superseded", res)
	}
	if got := onlyRecord(t, w.store); got.Status != StatusSuperseded {
		t.Fatalf("after the outage = %+v, want superseded", got)
	}
}
