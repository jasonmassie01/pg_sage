package rollout

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// executor-04b: a fleet canary's run state (targets, each target's part
// and verdict) is persisted as it runs, and a run a stopped sidecar left
// in canary is marked interrupted at the next startup, with no resume.
// The sweep interrupts every orphaned canary of its database, so each
// test here has a database of its own.

// freshRunPool is a new database with the sage schema.
func freshRunPool(t *testing.T, label string) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	pool := openPool(t, dsn)
	if err := schema.Bootstrap(context.Background(), pool); err != nil {
		t.Fatalf("bootstrap %s: %v", label, err)
	}
	return pool, dsn
}

// openPool is a second sidecar's pool on the same database.
func openPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// blockMeasure makes every verdict wait block until release is called.
func blockMeasure(svc *CanaryService) (release func()) {
	block := make(chan struct{})
	svc.wait = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-block:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var once sync.Once
	return func() { once.Do(func() { close(block) }) }
}

// waitForInstance waits until the run records database as status.
func waitForInstance(t *testing.T, runs *PostgresRunStore, runID, database,
	status string) InstanceRecord {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		instances, err := runs.Instances(context.Background(), runID)
		for _, inst := range instances {
			if err == nil && inst.InstanceID == database && inst.Status == status {
				return inst
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became %s: %+v (%v)", database, status, instances, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitIdle waits until the service has no canary running.
func waitIdle(t *testing.T, svc *CanaryService) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for svc.running.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the canary goroutine never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killRunSession ends the session that holds the run's lock, as a
// sidecar crash or restart would.
func killRunSession(t *testing.T, pool *pgxpool.Pool, runID string) {
	t.Helper()
	var killed int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM (
		SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objsubid = 2
		  AND classid = $1::int::oid AND objid = hashtext($2)::oid) k`,
		runLockNamespace, runID).Scan(&killed)
	if err != nil || killed != 1 {
		t.Fatalf("terminate the run's session: %d sessions (%v), want 1", killed, err)
	}
}

func runRow(t *testing.T, runs *PostgresRunStore, id string) RunRecord {
	t.Helper()
	run, err := runs.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("read run %s: %v", id, err)
	}
	return run
}

// State transition (DB): a run in canary when its sidecar stops is
// interrupted at the next startup, the target it was changing is
// recorded with its action, and no further target is touched.
func TestCanaryRestartMarksTheRunInterruptedAndTouchesNothing(t *testing.T) {
	pool, dsn := freshRunPool(t, "canary restart")
	f := newCanaryFixtureOn(t, pool, "a", "b")
	release := blockMeasure(f.svc)
	t.Cleanup(release)
	started, err := f.svc.Start(context.Background(), canaryRequest("a", "b"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := runRow(t, f.runs, started.EvidenceID); strings.Join(got.Targets, ",") != "a,b" {
		t.Fatalf("persisted targets = %v, want [a b]", got.Targets)
	}
	inst := waitForInstance(t, f.runs, started.EvidenceID, "a", StatusAwaitingVerdict)
	if inst.ActionLogID != 101 || inst.Ordinal != 1 {
		t.Fatalf("progress of a = %+v, want action 101 at ordinal 1", inst)
	}
	killRunSession(t, pool, started.EvidenceID)

	after := f.restartedService(t, openPool(t, dsn))
	got, err := after.RecoverInterrupted(context.Background())
	if err != nil || len(got) != 1 || got[0].Run.EvidenceID != started.EvidenceID {
		t.Fatalf("recover = %+v (%v), want the one run", got, err)
	}
	if got[0].Run.State != StateInterrupted || got[0].Run.HaltReason != HaltSidecarRestart ||
		len(got[0].Changing) != 1 || got[0].Changing[0].InstanceID != "a" ||
		got[0].Changing[0].ActionLogID != 101 {
		t.Fatalf("interrupted run = %+v", got[0])
	}
	stored, instances, err := after.Get(context.Background(), started.EvidenceID)
	if err != nil || stored.State != StateInterrupted || len(instances) != 1 ||
		instances[0].Status != StatusInterrupted ||
		!strings.Contains(instances[0].Detail, "restart") {
		t.Fatalf("stored = %+v, instances %+v (%v)", stored, instances, err)
	}
	if len(f.targets["b"].executed) != 0 || len(f.targets["a"].executed) != 1 ||
		len(f.targets["a"].rolledBack) != 0 {
		t.Fatalf("targets touched after the restart: a=%v/%v b=%v",
			f.targets["a"].executed, f.targets["a"].rolledBack, f.targets["b"].executed)
	}
	f.assertZombieStops(t, release, started.EvidenceID)
}

// assertZombieStops lets the old process (whose session died, but whose
// goroutine lives on in the test) continue: its next progress write is
// refused, so it rolls back only the change it made itself and never
// reaches b, and the run stays interrupted.
func (f *canaryFixture) assertZombieStops(t *testing.T, release func(), runID string) {
	t.Helper()
	release()
	waitIdle(t, f.svc)
	if run := runRow(t, f.runs, runID); run.State != StateInterrupted {
		t.Fatalf("the old process overwrote the interrupted run: %+v", run)
	}
	if len(f.targets["b"].executed) != 0 {
		t.Fatalf("the old process reached b: %v", f.targets["b"].executed)
	}
	if rb := f.targets["a"].rolledBack; len(rb) != 1 || rb[0] != 101 {
		t.Fatalf("the old process rolled back %v, want only its own action 101", rb)
	}
}

// restartedService is the canary service of a sidecar that started on
// pool, resolving the same targets.
func (f *canaryFixture) restartedService(t *testing.T, pool *pgxpool.Pool) *CanaryService {
	t.Helper()
	svc, err := NewCanaryService(f.svc.resolve, NewPostgresRunStore(pool),
		CanaryOptions{CanaryInstances: 1, RegressionLimitPct: 10, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// HA: a second sidecar starting while the first runs a canary leaves the
// live run alone; the run then completes normally.
func TestCanaryRecoverLeavesALiveRunOfAnotherSidecar(t *testing.T) {
	pool, dsn := freshRunPool(t, "canary live")
	f := newCanaryFixtureOn(t, pool, "a", "b")
	release := blockMeasure(f.svc)
	t.Cleanup(release)
	started, err := f.svc.Start(context.Background(), canaryRequest("a", "b"))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForInstance(t, f.runs, started.EvidenceID, "a", StatusAwaitingVerdict)
	got, err := f.restartedService(t, openPool(t, dsn)).RecoverInterrupted(t.Context())
	if err != nil || len(got) != 0 {
		t.Fatalf("recover on the second sidecar = %+v (%v), want nothing", got, err)
	}
	if run := runRow(t, f.runs, started.EvidenceID); run.State != "canary" {
		t.Fatalf("live run = %+v, want still canary", run)
	}
	release()
	waitIdle(t, f.svc)
	run, instances, err := f.svc.Get(context.Background(), started.EvidenceID)
	if err != nil || run.State != "complete" || len(instances) != 2 ||
		instances[0].Status != StatusApplied || instances[1].Status != StatusApplied {
		t.Fatalf("finished run = %+v, %+v (%v)", run, instances, err)
	}
}

// orphanedRun records a canary whose sidecar is gone: a canary row and
// a target awaiting its verdict, with no session holding its lock.
func orphanedRun(t *testing.T, runs *PostgresRunStore, id string) {
	t.Helper()
	now := time.Now().UTC()
	err := runs.Create(context.Background(), RunRecord{EvidenceID: id,
		SourceInstance: "source", PriorEvidenceID: "action_log:source:42",
		Policy: Policy{CanaryInstances: 1, MaxAffectedInstances: 2,
			RequireLocalReverification: true},
		State: "canary", Targets: []string{"a", "b"}, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("create orphaned run: %v", err)
	}
	err = runs.SaveProgress(context.Background(), id, InstanceRecord{InstanceID: "a",
		Ordinal: 1, Status: StatusAwaitingVerdict, ActionLogID: 7, UpdatedAt: now})
	if err != nil {
		t.Fatalf("record progress: %v", err)
	}
}

// Concurrency: two sidecars (HA) starting at once see one interrupted
// transition between them.
func TestCanaryRecoverOnTwoSidecarsInterruptsOnce(t *testing.T) {
	pool, dsn := freshRunPool(t, "canary ha")
	f := newCanaryFixtureOn(t, pool, "a", "b")
	orphanedRun(t, f.runs, "canary-orphan-ha")
	sidecars := []*CanaryService{f.restartedService(t, openPool(t, dsn)),
		f.restartedService(t, openPool(t, dsn))}
	var wg sync.WaitGroup
	gate := make(chan struct{})
	results := make([][]InterruptedRun, len(sidecars))
	errs := make([]error, len(sidecars))
	for i, svc := range sidecars {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			results[i], errs[i] = svc.RecoverInterrupted(context.Background())
		}()
	}
	close(gate)
	wg.Wait()
	total := 0
	for i := range sidecars {
		if errs[i] != nil {
			t.Fatalf("sidecar %d: %v", i, errs[i])
		}
		total += len(results[i])
	}
	if total != 1 {
		t.Fatalf("interrupted transitions = %d (%+v), want exactly 1", total, results)
	}
	_, instances, err := f.svc.Get(context.Background(), "canary-orphan-ha")
	if err != nil || len(instances) != 1 || instances[0].Status != StatusInterrupted ||
		instances[0].ActionLogID != 7 {
		t.Fatalf("instances = %+v (%v)", instances, err)
	}
}

// Nil/empty: no persisted runs means no work; a second sweep finds
// nothing left, and only canary runs are swept.
func TestCanaryRecoverWithNothingToRecover(t *testing.T) {
	pool, _ := freshRunPool(t, "canary empty")
	f := newCanaryFixtureOn(t, pool, "a")
	got, err := f.svc.RecoverInterrupted(context.Background())
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty recover = %#v (%v), want an empty, non-nil list", got, err)
	}
	run, err := f.svc.Run(context.Background(), canaryRequest("a"))
	if err != nil || run.State != "complete" {
		t.Fatalf("run = %+v (%v)", run, err)
	}
	orphanedRun(t, f.runs, "canary-orphan-once")
	if got, err := f.svc.RecoverInterrupted(t.Context()); err != nil || len(got) != 1 {
		t.Fatalf("first sweep = %+v (%v), want the orphan", got, err)
	}
	if got, err := f.svc.RecoverInterrupted(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("second sweep = %+v (%v), want nothing", got, err)
	}
	if done := runRow(t, f.runs, run.EvidenceID); done.State != "complete" {
		t.Fatalf("a complete run was swept: %+v", done)
	}
}

// Error propagation: a progress write the database refuses stops the
// canary before it changes the target.
func TestCanaryProgressWriteFailureTouchesNoTarget(t *testing.T) {
	pool, _ := freshRunPool(t, "canary progress fail")
	if _, err := pool.Exec(context.Background(), `ALTER TABLE sage.rollout_instance
		ADD CONSTRAINT no_applying CHECK (status <> 'applying')`); err != nil {
		t.Fatalf("add the refusing constraint: %v", err)
	}
	f := newCanaryFixtureOn(t, pool, "a", "b")
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b"))
	if err == nil || !strings.Contains(err.Error(), "progress") {
		t.Fatalf("run error = %v, want a progress write error", err)
	}
	if run.State != "failed" || len(f.targets["a"].executed) != 0 ||
		len(f.targets["b"].executed) != 0 {
		t.Fatalf("run = %+v, executed a=%v b=%v", run, f.targets["a"].executed,
			f.targets["b"].executed)
	}
}

// Error propagation: the sweep reports a database it cannot reach.
func TestCanaryRecoverReportsAnUnreachableStore(t *testing.T) {
	pool, _ := freshRunPool(t, "canary closed")
	f := newCanaryFixtureOn(t, pool, "a")
	pool.Close()
	_, err := f.svc.RecoverInterrupted(context.Background())
	if err == nil || !strings.Contains(err.Error(), "interrupt") {
		t.Fatalf("recover on a closed pool = %v, want an interrupt error", err)
	}
}

// Invalid input: progress for a run that is not canary is refused.
func TestSaveProgressRefusesARunThatIsNotRunning(t *testing.T) {
	pool := bootstrappedPool(t)
	runs := NewPostgresRunStore(pool)
	err := runs.SaveProgress(context.Background(), "canary-never-created",
		InstanceRecord{InstanceID: "a", Ordinal: 1, Status: StatusApplying,
			UpdatedAt: time.Now()})
	if !errors.Is(err, ErrRunInterrupted) {
		t.Fatalf("progress for an unknown run = %v, want ErrRunInterrupted", err)
	}
}

// State transition (DB): an interrupted run rolls nothing back, so the
// sweep reports the databases it had changed with a verdict as keeping
// the change, apart from those in flight, and leaves their records.
func TestCanaryRecoverReportsTheDatabasesThatKeepTheChange(t *testing.T) {
	pool, _ := freshRunPool(t, "canary kept")
	runs := NewPostgresRunStore(pool)
	now := time.Now().UTC()
	if err := runs.Create(context.Background(), RunRecord{EvidenceID: "canary-kept",
		SourceInstance: "source", PriorEvidenceID: "action_log:source:42",
		Policy: Policy{CanaryInstances: 1, MaxAffectedInstances: 3}, State: "canary",
		Targets: []string{"a", "b", "c"}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	for _, r := range []InstanceRecord{
		{InstanceID: "b", Ordinal: 2, Status: StatusAwaitingVerdict, ActionLogID: 6},
		{InstanceID: "a", Ordinal: 1, Status: StatusApplied, ActionLogID: 5},
		{InstanceID: "c", Ordinal: 3, Status: StatusNotLocallyVerified},
	} {
		r.UpdatedAt = now
		if err := runs.SaveProgress(context.Background(), "canary-kept", r); err != nil {
			t.Fatalf("record %s: %v", r.InstanceID, err)
		}
	}
	svc, err := NewCanaryService(func(string) (CanaryTarget, error) {
		return nil, errors.New("no target is touched by a sweep")
	}, runs, CanaryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.RecoverInterrupted(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("recover = %+v (%v), want one run", got, err)
	}
	r := got[0]
	if len(r.Changing) != 1 || r.Changing[0].InstanceID != "b" ||
		r.Changing[0].ActionLogID != 6 || len(r.Kept) != 1 || r.Kept[0].InstanceID != "a" ||
		r.Kept[0].ActionLogID != 5 || r.Kept[0].Status != StatusApplied {
		t.Fatalf("changing %+v, kept %+v; want b (6) in flight and a (5) kept",
			r.Changing, r.Kept)
	}
	_, instances, err := svc.Get(context.Background(), "canary-kept")
	if err != nil || len(instances) != 3 || instances[0].Status != StatusApplied ||
		instances[1].Status != StatusInterrupted ||
		instances[2].Status != StatusNotLocallyVerified {
		t.Fatalf("stored instances = %+v (%v)", instances, err)
	}
}
