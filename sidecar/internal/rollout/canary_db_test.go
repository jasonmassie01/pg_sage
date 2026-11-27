package rollout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The fleet canary for remediations (AI-SRE-SPEC §4 R3, R06): an action
// verified on one database is offered to databases whose own analyzer
// recommends the same change (local re-verification), applied there as
// the operator's approval through each database's gate, one database
// first, and halted and rolled back everywhere on a failed verification.

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/rollout"))
}

var (
	poolOnce   sync.Once
	sharedPool *pgxpool.Pool
	poolErr    error
)

// bootstrappedPool is the package fixture database with the sage schema.
func bootstrappedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	poolOnce.Do(func() {
		sharedPool, poolErr = pgxpool.New(context.Background(), dsn)
		if poolErr == nil {
			poolErr = schema.Bootstrap(context.Background(), sharedPool)
		}
	})
	if poolErr != nil {
		t.Fatalf("test database: %v", poolErr)
	}
	return sharedPool
}

// fakeTarget is one fleet database: its open recommendations, the
// actions applied there and their verification results.
type fakeTarget struct {
	mu         sync.Mutex
	name       string
	findings   map[string]int // sql -> finding id
	source     map[int64]SourceAction
	results    map[int64][2]string
	nextAction int64
	executed   []string
	rolledBack []int64
	failVerify bool
	// A real target records a reversible action as "monitoring" until its
	// monitor's verdict (executor-04): the fake reports the final verdict
	// only after settleAfter reads of it.
	settleAfter int
	final       map[int64][2]string
	reads       map[int64]int
	revert      bool
	neverSettle bool
	deadline    time.Duration // monitor deadline, relative to now
	executeErr  error
	rollbackErr error
}

func newTarget(name string, sql string) *fakeTarget {
	t := &fakeTarget{name: name, findings: map[string]int{}, source: map[int64]SourceAction{},
		results: map[int64][2]string{}, nextAction: 100, settleAfter: 2,
		final: map[int64][2]string{}, reads: map[int64]int{}, deadline: time.Hour}
	if sql != "" {
		t.findings[sql] = 7
	}
	return t
}

func (t *fakeTarget) Name() string { return t.name }

func (t *fakeTarget) SourceAction(_ context.Context, id int64) (SourceAction, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.source[id]
	if !ok {
		return SourceAction{}, ErrSourceNotVerified
	}
	return a, nil
}

func (t *fakeTarget) MatchingFinding(_ context.Context, sql string) (int, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.findings[sql]
	return id, ok, nil
}

func (t *fakeTarget) Execute(_ context.Context, findingID int, sql, _ string,
	_ *int) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if findingID != t.findings[sql] {
		return 0, fmt.Errorf("finding %d does not recommend %q", findingID, sql)
	}
	if t.executeErr != nil {
		return 0, t.executeErr
	}
	t.nextAction++
	t.executed = append(t.executed, sql)
	final := [2]string{"success", "success"}
	switch {
	case t.failVerify:
		final = [2]string{"failed", "failed"}
	case t.revert:
		final = [2]string{"rolled_back", "revert"}
	}
	t.results[t.nextAction] = [2]string{"monitoring", "pending"}
	t.final[t.nextAction] = final
	return t.nextAction, nil
}

func (t *fakeTarget) Rollback(_ context.Context, actionLogID int64, _ string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rolledBack = append(t.rolledBack, actionLogID)
	return t.rollbackErr
}

func (t *fakeTarget) MonitorDeadline(_ context.Context, actionLogID int64) (time.Time,
	error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.results[actionLogID]; !ok {
		return time.Time{}, errors.New("no such action")
	}
	return time.Now().Add(t.deadline), nil
}

func (t *fakeTarget) ActionResult(_ context.Context, actionLogID int64) (string, string,
	error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.results[actionLogID]
	if !ok {
		return "", "", errors.New("no such action")
	}
	t.reads[actionLogID]++
	if final, ok := t.final[actionLogID]; ok && !t.neverSettle &&
		t.reads[actionLogID] > t.settleAfter {
		r = final
		t.results[actionLogID] = r
	}
	return r[0], r[1], nil
}

const canarySQL = "CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders (status)"

type canaryFixture struct {
	targets map[string]*fakeTarget
	svc     *CanaryService
	runs    *PostgresRunStore
}

func newCanaryFixture(t *testing.T, names ...string) *canaryFixture {
	t.Helper()
	return newCanaryFixtureOn(t, bootstrappedPool(t), names...)
}

// newCanaryFixtureOn is newCanaryFixture on a database of the caller's.
func newCanaryFixtureOn(t *testing.T, pool *pgxpool.Pool, names ...string) *canaryFixture {
	t.Helper()
	f := &canaryFixture{targets: map[string]*fakeTarget{}, runs: NewPostgresRunStore(pool)}
	src := newTarget("source", "")
	src.source[42] = SourceAction{SQL: canarySQL,
		RollbackSQL: "DROP INDEX CONCURRENTLY public.idx_orders_status",
		ActionType:  "create_index_concurrently", Outcome: "success", Verification: "success"}
	f.targets["source"] = src
	for _, n := range names {
		f.targets[n] = newTarget(n, canarySQL)
	}
	svc, err := NewCanaryService(func(name string) (CanaryTarget, error) {
		target, ok := f.targets[name]
		if !ok {
			return nil, fmt.Errorf("database %q is not in the fleet", name)
		}
		return target, nil
	}, f.runs, CanaryOptions{CanaryInstances: 1, RegressionLimitPct: 10,
		PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func canaryRequest(targets ...string) StartRequest {
	return StartRequest{SourceDatabase: "source", ActionLogID: 42, Targets: targets,
		StartedBy: "user:1:admin@example.com", Family: "plan_regression",
		Class: "index_create"}
}

func TestCanaryAppliesOneFirstThenWidens(t *testing.T) {
	f := newCanaryFixture(t, "a", "b", "c")
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "complete" || run.AppliedInstances != 3 ||
		strings.Join(run.CanaryInstanceIDs, ",") != "a" || run.StartedBy !=
		"user:1:admin@example.com" || run.Family != "plan_regression" {
		t.Fatalf("run = %+v", run)
	}
	got, instances, err := f.svc.Get(context.Background(), run.EvidenceID)
	if err != nil || got.State != "complete" || len(instances) != 3 {
		t.Fatalf("stored run = %+v, %d instances (%v)", got, len(instances), err)
	}
	for i, inst := range instances {
		if inst.Ordinal != i+1 || inst.Status != StatusApplied || inst.ActionLogID == 0 {
			t.Fatalf("instance %d = %+v", i, inst)
		}
	}
	if instances[0].InstanceID != "a" {
		t.Fatalf("first instance = %s, want the canary a", instances[0].InstanceID)
	}
}

func TestCanaryHaltsAndRollsBackOnFailedVerification(t *testing.T) {
	f := newCanaryFixture(t, "a", "b", "c")
	f.targets["b"].failVerify = true
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "halted" || run.HaltReason != "verification_failed" {
		t.Fatalf("run = %+v", run)
	}
	if len(f.targets["a"].rolledBack) != 1 || len(f.targets["b"].rolledBack) != 1 ||
		len(f.targets["c"].executed) != 0 {
		t.Fatalf("rollbacks a=%v b=%v, c executed %v", f.targets["a"].rolledBack,
			f.targets["b"].rolledBack, f.targets["c"].executed)
	}
	_, instances, err := f.svc.Get(context.Background(), run.EvidenceID)
	if err != nil || len(instances) != 2 || instances[0].Status != StatusRolledBack ||
		instances[1].Status != StatusRolledBack {
		t.Fatalf("instances = %+v (%v)", instances, err)
	}
}

func TestCanarySkipsDatabasesWithoutTheRecommendation(t *testing.T) {
	f := newCanaryFixture(t, "a", "b", "c")
	delete(f.targets["b"].findings, canarySQL)
	run, err := f.svc.Run(context.Background(), canaryRequest("a", "b", "c"))
	if err != nil || run.State != "complete" || run.AppliedInstances != 2 {
		t.Fatalf("run = %+v (%v)", run, err)
	}
	_, instances, _ := f.svc.Get(context.Background(), run.EvidenceID)
	statuses := map[string]string{}
	for _, inst := range instances {
		statuses[inst.InstanceID] = inst.Status
	}
	if statuses["b"] != StatusNotLocallyVerified || statuses["a"] != StatusApplied ||
		len(f.targets["b"].executed) != 0 {
		t.Fatalf("statuses = %v", statuses)
	}
}

func TestCanaryRequiresAVerifiedSourceWithARollback(t *testing.T) {
	f := newCanaryFixture(t, "a")
	src := f.targets["source"]
	for name, mutate := range map[string]func(*SourceAction){
		"failed":          func(a *SourceAction) { a.Outcome = "failed" },
		"rolled back":     func(a *SourceAction) { a.Outcome = "rolled_back" },
		"bad verdict":     func(a *SourceAction) { a.Verification = "revert" },
		"still verifying": func(a *SourceAction) { a.Verification = "pending" },
		"no rollback":     func(a *SourceAction) { a.RollbackSQL = "" },
	} {
		a := src.source[42]
		mutate(&a)
		src.source[43] = a
		req := canaryRequest("a")
		req.ActionLogID = 43
		if _, err := f.svc.Run(context.Background(), req); !errors.Is(err,
			ErrSourceNotVerified) {
			t.Errorf("%s: %v", name, err)
		}
	}
	req := canaryRequest("a")
	req.SourceDatabase = "nowhere"
	if _, err := f.svc.Run(context.Background(), req); err == nil {
		t.Fatal("an unknown source database was accepted")
	}
	if len(f.targets["a"].executed) != 0 {
		t.Fatal("an unverified source reached a target")
	}
}

func TestCanaryValidatesTheRequest(t *testing.T) {
	f := newCanaryFixture(t, "a", "b")
	for name, req := range map[string]StartRequest{
		"no targets": canaryRequest(),
		"duplicate":  canaryRequest("a", "a"),
		"source too": canaryRequest("a", "source"),
		"not a human": func() StartRequest {
			r := canaryRequest("a")
			r.StartedBy = "pg_sage"
			return r
		}(),
		"no action id": func() StartRequest { r := canaryRequest("a"); r.ActionLogID = 0; return r }(),
		"unknown family": func() StartRequest {
			r := canaryRequest("a")
			r.Family = "shell"
			return r
		}(),
	} {
		if _, err := f.svc.Run(context.Background(), req); !errors.Is(err, ErrInvalidCanary) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCanaryStartIsAsyncAndSingleFlight(t *testing.T) {
	f := newCanaryFixture(t, "a", "b")
	block := make(chan struct{})
	f.svc.wait = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-block:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	run, err := f.svc.Start(context.Background(), canaryRequest("a", "b"))
	if err != nil || run.State != "canary" || run.EvidenceID == "" {
		t.Fatalf("start = %+v (%v)", run, err)
	}
	if _, err := f.svc.Start(context.Background(), canaryRequest("a", "b")); !errors.Is(err,
		ErrCanaryRunning) {
		t.Fatalf("second start while running: %v", err)
	}
	close(block)
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, _, err := f.svc.Get(context.Background(), run.EvidenceID)
		if err == nil && got.State == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("async run never completed: %+v (%v)", got, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	list, err := f.svc.List(context.Background(), 5)
	if err != nil || len(list) == 0 || list[0].EvidenceID != run.EvidenceID {
		t.Fatalf("list = %+v (%v)", list, err)
	}
}
