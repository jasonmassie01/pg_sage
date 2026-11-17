package rollout

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

// lab-03b through the run store: the rollout halts on a measured
// regression past regression_limit_pct (rolling back newest first), the
// stored rollout_instance rows carry the effect, a restart keeps it, and
// a stage wait pauses before each next member.

// effectFleet is a canary service over effect targets.
type effectFleet struct {
	svc     *CanaryService
	runs    *PostgresRunStore
	members map[string]*effectTarget
	order   *callOrder
	waits   *waitLog
}

type waitLog struct {
	mu sync.Mutex
	d  []time.Duration
}

func (w *waitLog) all() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.d...)
}

func newEffectFleet(t *testing.T, pool *pgxpool.Pool, opts CanaryOptions,
	targets ...*effectTarget) *effectFleet {
	t.Helper()
	f := &effectFleet{runs: NewPostgresRunStore(pool), members: map[string]*effectTarget{},
		order: &callOrder{}, waits: &waitLog{}}
	src := newTarget("source", "")
	src.source[42] = SourceAction{SQL: canarySQL,
		RollbackSQL: "DROP INDEX CONCURRENTLY public.idx_orders_status",
		ActionType:  "create_index_concurrently", Outcome: "success", Verification: "success"}
	for _, target := range targets {
		target.order = f.order
		f.members[target.name] = target
	}
	if opts.CanaryInstances == 0 {
		opts.CanaryInstances, opts.RegressionLimitPct = 1, 10
	}
	opts.PollEvery = time.Millisecond
	svc, err := NewCanaryService(func(name string) (CanaryTarget, error) {
		if name == "source" {
			return src, nil
		}
		target, ok := f.members[name]
		if !ok {
			return nil, fmt.Errorf("database %q is not in the fleet", name)
		}
		return target, nil
	}, f.runs, opts)
	if err != nil {
		t.Fatal(err)
	}
	svc.wait = func(ctx context.Context, d time.Duration) error {
		f.waits.mu.Lock()
		f.waits.d = append(f.waits.d, d)
		f.waits.mu.Unlock()
		return ctx.Err()
	}
	f.svc = svc
	return f
}

func (f *effectFleet) instances(t *testing.T, runID string) map[string]InstanceRecord {
	t.Helper()
	list, err := f.runs.Instances(context.Background(), runID)
	if err != nil {
		t.Fatalf("read instances: %v", err)
	}
	out := map[string]InstanceRecord{}
	for _, inst := range list {
		out[inst.InstanceID] = inst
	}
	return out
}

// Happy path end to end: a improves 30% with flat untreated members; the
// rollout completes and every row carries its effect and method.
func TestEffectRolloutStoresEachMembersEffect(t *testing.T) {
	a, b, c := newEffectTarget("a", -28, -30), newEffectTarget("b", 1, 0),
		newEffectTarget("c", 2, 0)
	f := newEffectFleet(t, bootstrappedPool(t), CanaryOptions{}, a, b, c)
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b", "c"))
	if err != nil || run.State != "complete" || run.AppliedInstances != 3 {
		t.Fatalf("run = %+v (%v)", run, err)
	}
	if run.AggregateRegressionPct != -30 {
		t.Fatalf("canary aggregate = %v, want the measured -30", run.AggregateRegressionPct)
	}
	got := f.instances(t, run.EvidenceID)
	if r := got["a"]; r.EffectMethod != EffectFleetControl || !near(r.EffectPct, -30) ||
		!near(r.ControlPct, 0) || r.RegressionPct != -30 || r.Status != StatusApplied {
		t.Fatalf("a = %+v (effect %s)", r, fmtPtr(r.EffectPct))
	}
	// b is measured while only c is untreated: one control is none.
	if r := got["b"]; r.EffectMethod != EffectOwnV2 || !near(r.EffectPct, 1) ||
		r.ControlPct != nil {
		t.Fatalf("b = %+v", r)
	}
	if r := got["c"]; r.EffectMethod != EffectOwnV2 || !near(r.EffectPct, 2) {
		t.Fatalf("c = %+v", r)
	}
	var effect float64
	var method string
	if err := bootstrappedPool(t).QueryRow(context.Background(), `SELECT effect_pct,
		effect_method FROM sage.rollout_instance WHERE run_evidence_id = $1
		AND instance_id = 'a'`, run.EvidenceID).Scan(&effect, &method); err != nil ||
		effect != -30 || method != EffectFleetControl {
		t.Fatalf("stored a = %v %q (%v)", effect, method, err)
	}
}

// Boundary: an effect exactly at regression_limit_pct does not halt;
// above it halts and rolls back newest first.
func TestEffectAtTheLimitProceedsAboveItHaltsAndRollsBack(t *testing.T) {
	a, b, c := newEffectTarget("a", 10, 0), newEffectTarget("b", 10.5, 0),
		newEffectTarget("c", 0, 0)
	a.ran, b.ran, c.ran = false, false, false
	f := newEffectFleet(t, bootstrappedPool(t), CanaryOptions{}, a, b, c)
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b", "c"))
	if err != nil || run.State != "halted" || run.HaltReason != "instance_regression" {
		t.Fatalf("run = %+v (%v), want halted on b's measured regression", run, err)
	}
	if got := f.order.String(); got != "b,a" {
		t.Fatalf("rollback order = %q, want newest first b,a", got)
	}
	if len(c.executed) != 0 {
		t.Fatalf("c was changed after the halt: %v", c.executed)
	}
	got := f.instances(t, run.EvidenceID)
	if r := got["b"]; r.Status != StatusRolledBack || !near(r.EffectPct, 10.5) ||
		r.RegressionPct != 10.5 || r.EffectMethod != EffectOwnV2 {
		t.Fatalf("b = %+v", r)
	}
	if r := got["a"]; r.Status != StatusRolledBack || !near(r.EffectPct, 10) {
		t.Fatalf("a = %+v", r)
	}
}

// Boundary on the canary: a canary above the limit halts as an aggregate
// regression before anything widens.
func TestEffectCanaryAboveTheLimitHaltsAsAggregate(t *testing.T) {
	a, b := newEffectTarget("a", 10.000001, 0), newEffectTarget("b", 0, 0)
	a.ran, b.ran = false, false
	f := newEffectFleet(t, bootstrappedPool(t), CanaryOptions{}, a, b)
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b"))
	if err != nil || run.HaltReason != "aggregate_regression" || len(b.executed) != 0 ||
		f.order.String() != "a" {
		t.Fatalf("run = %+v (%v), b %v, rollbacks %q", run, err, b.executed, f.order)
	}
}

// State transition: a flag-only member (no v2 number) still completes on
// its kept success, and its row says flag_only.
func TestEffectFlagOnlyMemberIsStoredAsFlagOnly(t *testing.T) {
	a := newEffectTarget("a", 0, 0)
	a.effect = MemberEffect{Malformed: true, Reason: "the v2 record is an array, " +
		"not a JSON object"}
	f := newEffectFleet(t, bootstrappedPool(t), CanaryOptions{}, a)
	run, err := f.svc.Run(context.Background(), canaryRequest("a"))
	if err != nil || run.State != "complete" {
		t.Fatalf("run = %+v (%v)", run, err)
	}
	r := f.instances(t, run.EvidenceID)["a"]
	if r.EffectMethod != EffectFlagOnly || r.EffectPct != nil ||
		!strings.Contains(r.Detail, "flag only") {
		t.Fatalf("a = %+v", r)
	}
}

// Pause between stages: stage_wait waits before each next member, never
// before the first and never after the last.
func TestEffectStageWaitPausesBeforeEachNextMember(t *testing.T) {
	a, b, c := newEffectTarget("a", 0, 0), newEffectTarget("b", 0, 0),
		newEffectTarget("c", 0, 0)
	f := newEffectFleet(t, bootstrappedPool(t), CanaryOptions{CanaryInstances: 1,
		RegressionLimitPct: 10, StageWait: 24 * time.Hour}, a, b, c)
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b", "c"))
	if err != nil || run.State != "complete" {
		t.Fatalf("run = %+v (%v)", run, err)
	}
	waits := f.waits.all()
	if len(waits) != 2 || waits[0] != 24*time.Hour || waits[1] != 24*time.Hour {
		t.Fatalf("stage waits = %v, want two 24h pauses (before b and c)", waits)
	}
}

// Error propagation: a pause the caller cancels stops the rollout with
// the context's error and rolls back what was changed.
func TestEffectCancelledStageWaitRollsBack(t *testing.T) {
	a, b := newEffectTarget("a", 0, 0), newEffectTarget("b", 0, 0)
	f := newEffectFleet(t, bootstrappedPool(t), CanaryOptions{CanaryInstances: 1,
		RegressionLimitPct: 10, StageWait: time.Hour}, a, b)
	f.svc.wait = func(context.Context, time.Duration) error { return context.Canceled }
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b"))
	if !errors.Is(err, context.Canceled) || run.State != "failed" ||
		f.order.String() != "a" || len(b.executed) != 0 {
		t.Fatalf("run = %+v (%v), rollbacks %q, b %v", run, err, f.order, b.executed)
	}
}

// State transition: a restart keeps the effect stored for a member whose
// verdict was recorded before the sidecar stopped.
func TestEffectRestartKeepsTheStoredEffect(t *testing.T) {
	pool, dsn := freshRunPool(t, "effect restart")
	a, b := newEffectTarget("a", -12, 0), newEffectTarget("b", 0, 0)
	a.ran = false
	b.settleAfter = 2
	f := newEffectFleet(t, pool, CanaryOptions{}, a, b)
	release := blockMeasure(f.svc)
	t.Cleanup(release)
	started, err := f.svc.Start(context.Background(), canaryRequest("a", "b"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForInstance(t, f.runs, started.EvidenceID, "b", StatusAwaitingVerdict)
	killRunSession(t, pool, started.EvidenceID)
	after, err := NewCanaryService(f.svc.resolve, NewPostgresRunStore(openPool(t, dsn)),
		CanaryOptions{CanaryInstances: 1, RegressionLimitPct: 10})
	if err != nil {
		t.Fatal(err)
	}
	got, err := after.RecoverInterrupted(context.Background())
	if err != nil || len(got) != 1 || len(got[0].Kept) != 1 {
		t.Fatalf("recover = %+v (%v)", got, err)
	}
	if k := got[0].Kept[0]; k.InstanceID != "a" || !near(k.EffectPct, -12) ||
		k.EffectMethod != EffectOwnV2 {
		t.Fatalf("kept a = %+v (effect %s)", k, fmtPtr(k.EffectPct))
	}
	_, instances, err := after.Get(context.Background(), started.EvidenceID)
	if err != nil || len(instances) != 2 || !near(instances[0].EffectPct, -12) ||
		instances[1].EffectPct != nil || instances[1].Status != StatusInterrupted {
		t.Fatalf("stored instances = %+v (%v)", instances, err)
	}
	release()
	waitIdle(t, f.svc)
}

// Concurrency: a second canary started while one runs is refused by the
// one-at-a-time guard (asserted, not changed).
func TestEffectSecondCanaryWhileOneRunsIsRefused(t *testing.T) {
	a := newEffectTarget("a", 0, 0)
	a.settleAfter = 2
	f := newEffectFleet(t, bootstrappedPool(t), CanaryOptions{}, a)
	release := blockMeasure(f.svc)
	started, err := f.svc.Start(context.Background(), canaryRequest("a"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := f.svc.Start(context.Background(), canaryRequest("a")); !errors.Is(err,
		ErrCanaryRunning) {
		t.Fatalf("second start = %v, want ErrCanaryRunning", err)
	}
	if _, err := f.svc.Run(context.Background(), canaryRequest("a")); !errors.Is(err,
		ErrCanaryRunning) {
		t.Fatalf("second run = %v, want ErrCanaryRunning", err)
	}
	release()
	waitIdle(t, f.svc)
	if run := runRow(t, f.runs, started.EvidenceID); run.State != "complete" {
		t.Fatalf("first run = %+v", run)
	}
}

// Concurrency: effects written for many members at once land on each
// member's own row.
func TestEffectParallelProgressWritesKeepTheirOwnRow(t *testing.T) {
	pool, _ := freshRunPool(t, "effect parallel")
	runs := NewPostgresRunStore(pool)
	const id = "canary-effect-parallel"
	now := time.Now().UTC()
	if err := runs.Create(context.Background(), RunRecord{EvidenceID: id,
		SourceInstance: "source", PriorEvidenceID: "action_log:source:42", State: "canary",
		CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			effect, control := float64(i)*1.5, float64(-i)
			errs[i] = runs.SaveProgress(context.Background(), id, InstanceRecord{
				InstanceID: fmt.Sprintf("db%d", i), Ordinal: i + 1, Status: StatusApplied,
				EffectPct: &effect, ControlPct: &control, EffectMethod: EffectFleetControl,
				UpdatedAt: now})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	got, err := runs.Instances(context.Background(), id)
	if err != nil || len(got) != 8 {
		t.Fatalf("instances = %+v (%v)", got, err)
	}
	for i, r := range got {
		if r.InstanceID != fmt.Sprintf("db%d", i) || !near(r.EffectPct, float64(i)*1.5) ||
			!near(r.ControlPct, float64(-i)) || r.EffectMethod != EffectFleetControl {
			t.Fatalf("row %d = %+v", i, r)
		}
	}
}
