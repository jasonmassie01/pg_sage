package earned

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// CHECK-40: autonomy auto-downgrades to at most L1 on error-budget burn,
// failover (or an unknown HA role), stale evidence and a concurrent
// pg_sage action on the same object, and after a family safety
// regression. A downgrade is immediate and logged; the granted level is
// untouched, so the pair returns to it only while its evidence still
// holds.

type fakeBudget struct {
	state BudgetState
	err   error
	calls int
	mu    sync.Mutex
}

func (b *fakeBudget) ErrorBudget(context.Context, string) (BudgetState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	return b.state, b.err
}

type fakeHA struct {
	state HAState
	err   error
}

func (h *fakeHA) HAStatus(context.Context) (HAState, error) { return h.state, h.err }

type fakeConcurrency struct {
	mu        sync.Mutex
	count     int
	err       error
	targets   []string
	leaseHeld bool
	window    time.Duration
}

func (c *fakeConcurrency) ConcurrentActions(_ context.Context, targets []string,
	leaseHeld bool, window time.Duration) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.targets, c.leaseHeld, c.window = targets, leaseHeld, window
	return c.count, c.err
}

type limiterFixture struct {
	*fixture
	budget *fakeBudget
	ha     *fakeHA
	conc   *fakeConcurrency
	lim    *Limiter
}

func newLimiterFixture(t *testing.T) *limiterFixture {
	f := newFixture(t)
	lf := &limiterFixture{fixture: f,
		budget: &fakeBudget{state: BudgetState{Configured: true}},
		ha:     &fakeHA{state: HAState{Role: RolePrimary}},
		conc:   &fakeConcurrency{}}
	lf.lim = f.svc.Limiter(Binding{Database: "orders", Budget: lf.budget, HA: lf.ha,
		Concurrency: lf.conc})
	return lf
}

func freezeRequest(at time.Time) policy.ActionRequest {
	return policy.ActionRequest{IncidentFamily: string(FamilyWraparound),
		Feature: "freeze", SQL: "VACUUM (FREEZE) public.orders",
		TargetObjs: []string{"public.orders"}, EvidenceObservedAt: at,
		Contract: &policy.ActionContract{ActionType: "vacuum_table",
			RiskTier: policy.RiskSafe, RollbackClass: policy.RollbackNoRollbackNeeded}}
}

func (lf *limiterFixture) limit(req policy.ActionRequest) policy.AutonomyLimit {
	lf.t.Helper()
	got, err := lf.lim.Limit(lf.ctx, req)
	if err != nil {
		lf.t.Fatalf("Limit: %v", err)
	}
	return got
}

func TestLimitAtL3WithEverythingClear(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL3()
	got := lf.limit(freezeRequest(lf.clock.Now()))
	if got.Level != 3 || got.Granted != 3 || got.Downgraded || len(got.Reasons) != 0 ||
		got.Class != string(ClassFreeze) {
		t.Fatalf("limit = %+v", got)
	}
	if !slices.Equal(lf.conc.targets, []string{"public.orders"}) || lf.conc.leaseHeld ||
		lf.conc.window != 15*time.Minute {
		t.Fatalf("concurrency query = %v %v %v", lf.conc.targets, lf.conc.leaseHeld,
			lf.conc.window)
	}
}

func TestEachDowngradeSignalCapsAtL1(t *testing.T) {
	cases := map[string]struct {
		mutate func(*limiterFixture, *policy.ActionRequest)
		reason string
	}{
		"fast burn": {func(lf *limiterFixture, _ *policy.ActionRequest) {
			lf.budget.state.FastBurning = true
		}, DowngradeBudgetBurn},
		"budget unknown": {func(lf *limiterFixture, _ *policy.ActionRequest) {
			lf.budget.state.Unknown = true
		}, DowngradeBudgetUnknown},
		"budget error": {func(lf *limiterFixture, _ *policy.ActionRequest) {
			lf.budget.err = errors.New("slo store down")
		}, DowngradeBudgetUnavailable},
		"replica": {func(lf *limiterFixture, _ *policy.ActionRequest) {
			lf.ha.state.Role = RoleReplica
		}, DowngradeHARole},
		"role unknown": {func(lf *limiterFixture, _ *policy.ActionRequest) {
			lf.ha.state.Role = RoleUnknown
		}, DowngradeHARole},
		"safe mode": {func(lf *limiterFixture, _ *policy.ActionRequest) {
			lf.ha.state.SafeMode = true
		}, DowngradeFailover},
		"recent failover": {func(lf *limiterFixture, _ *policy.ActionRequest) {
			lf.ha.state.LastRoleChange = lf.clock.Now().Add(-29 * time.Minute)
		}, DowngradeFailover},
		"ha error": {func(lf *limiterFixture, _ *policy.ActionRequest) {
			lf.ha.err = errors.New("probe timeout")
		}, DowngradeHARole},
		"no evidence time": {func(_ *limiterFixture, r *policy.ActionRequest) {
			r.EvidenceObservedAt = time.Time{}
		}, DowngradeStaleEvidence},
		"old evidence": {func(lf *limiterFixture, r *policy.ActionRequest) {
			r.EvidenceObservedAt = lf.clock.Now().Add(-5*time.Minute - time.Second)
		}, DowngradeStaleEvidence},
		"future evidence": {func(lf *limiterFixture, r *policy.ActionRequest) {
			r.EvidenceObservedAt = lf.clock.Now().Add(10 * time.Minute)
		}, DowngradeStaleEvidence},
		"concurrent action": {func(lf *limiterFixture, _ *policy.ActionRequest) {
			lf.conc.count = 1
		}, DowngradeConcurrent},
		"concurrency error": {func(lf *limiterFixture, _ *policy.ActionRequest) {
			lf.conc.err = errors.New("lease table locked")
		}, DowngradeConcurrencyUnknown},
		"no target": {func(_ *limiterFixture, r *policy.ActionRequest) {
			r.TargetObjs = nil
		}, DowngradeConcurrencyUnknown},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			lf := newLimiterFixture(t)
			lf.seedL3()
			req := freezeRequest(lf.clock.Now())
			c.mutate(lf, &req)
			got := lf.limit(req)
			if got.Level > 1 || !got.Downgraded || !slices.Contains(got.Reasons, c.reason) ||
				got.Granted != 3 {
				t.Fatalf("limit = %+v, want <= L1 with %s", got, c.reason)
			}
			if lf.granted(FamilyWraparound, ClassFreeze) != L3 {
				t.Fatal("a transient downgrade changed the granted level")
			}
		})
	}
}

func TestDowngradeBoundariesAreExclusive(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL3()
	req := freezeRequest(lf.clock.Now().Add(-5 * time.Minute))
	lf.ha.state.LastRoleChange = lf.clock.Now().Add(-30 * time.Minute)
	if got := lf.limit(req); got.Level != 3 || got.Downgraded {
		t.Fatalf("evidence exactly 5 min old and failover exactly 30 min ago: %+v", got)
	}
}

func TestMissingSignalSourcesFailClosed(t *testing.T) {
	f := newFixture(t)
	f.seedL3()
	lim := f.svc.Limiter(Binding{Database: "orders"})
	got, err := lim.Limit(f.ctx, freezeRequest(f.clock.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Level > 1 || !slices.Contains(got.Reasons, DowngradeHARole) ||
		!slices.Contains(got.Reasons, DowngradeConcurrencyUnknown) ||
		slices.Contains(got.Reasons, DowngradeBudgetUnavailable) {
		t.Fatalf("limit without sources = %+v (no SLO subsystem is not a burn)", got)
	}
}

func TestBudgetWithoutSLOsIsNotABurn(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL3()
	lf.budget.state = BudgetState{Configured: false, Unknown: true}
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level != 3 {
		t.Fatalf("no SLOs configured: %+v", got)
	}
}

func TestSafetyRegressionDemotesThroughTheService(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL3()
	if err := lf.svc.RecordOutcome(lf.ctx, Outcome{Database: "orders", Family: FamilyWraparound,
		Class: ClassFreeze, Level: L3, Result: ResultSafetyViolation,
		Source: SourceGameDay, Actor: ActorPgSage}); err != nil {
		t.Fatal(err)
	}
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level > 1 || got.Granted != 1 {
		t.Fatalf("limit after a safety violation = %+v", got)
	}
}

// Defense in depth: a violation in the window caps the pair even if the
// durable demotion never happened (here the outcome is written behind
// the service's back).
func TestSafetyRegressionInWindowCapsWithoutDemotion(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL3()
	if _, err := lf.pool.Exec(lf.ctx, `INSERT INTO sage.sre_autonomy_outcomes
		(deployment_id, database_name, family, action_class, level, result, source,
		 actor, recorded_at)
		VALUES ($1, 'orders', 'wraparound_runway', 'vacuum', 1, 'harmful', 'operator',
		        'user:2:o@e', $2)`, lf.store.DeploymentID(),
		lf.clock.Now().Add(-29*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := lf.limit(freezeRequest(lf.clock.Now()))
	if got.Level > 1 || got.Granted != 3 || !slices.Contains(got.Reasons,
		DowngradeSafetyRegression) {
		t.Fatalf("limit = %+v", got)
	}
	lf.clock.Advance(2 * 24 * time.Hour)
	if got := lf.limit(freezeRequest(lf.clock.Now())); slices.Contains(got.Reasons,
		DowngradeSafetyRegression) {
		t.Fatalf("a violation 31 days old still caps: %+v", got)
	}
}

func TestCapsApplyBeforeAnyGrant(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL3()
	req := freezeRequest(lf.clock.Now())
	req.Contract.RollbackClass = policy.RollbackNotReversible
	if got := lf.limit(req); got.Level > 1 {
		t.Fatalf("irreversible contract on an L3 pair: %+v", got)
	}
	req = freezeRequest(lf.clock.Now())
	req.Contract.RollbackClass = policy.RollbackClass("mitigation_only")
	if got := lf.limit(req); got.Level > 2 {
		t.Fatalf("mitigation-only contract on an L3 pair: %+v", got)
	}
	req = freezeRequest(lf.clock.Now())
	req.Contract = nil
	if got := lf.limit(req); got.Level > 1 {
		t.Fatalf("no contract: %+v", got)
	}
	req = freezeRequest(lf.clock.Now())
	req.IncidentFamily = "shell"
	if got := lf.limit(req); got.Level != 0 {
		t.Fatalf("unknown family: %+v", got)
	}
	req = freezeRequest(lf.clock.Now())
	req.IncidentFamily = string(FamilyLockBlocking)
	if got := lf.limit(req); got.Level > 1 {
		t.Fatalf("freeze is not a lock_blocking class: %+v", got)
	}
}

// The ledger also re-checks the evidence behind a granted level: when it
// decays the effective level drops with it, without a human.
func TestDecayedEvidenceLowersTheEffectiveLevel(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL3()
	lf.clock.Advance(31 * 24 * time.Hour)
	got := lf.limit(freezeRequest(lf.clock.Now()))
	if got.Level > 1 || got.Granted != 3 {
		t.Fatalf("limit with a 31-day-old bench and an empty shadow window = %+v", got)
	}
}

func TestLeaseHeldIsPassedThrough(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL3()
	req := freezeRequest(lf.clock.Now())
	req.LeaseHeld = true
	lf.limit(req)
	if !lf.conc.leaseHeld {
		t.Fatal("re-authorization under our own lease must ignore that lease")
	}
}

// Caps are logged once when they start and once when they clear, per
// database and pair, not on every authorization.
func TestCapTransitionsAreLoggedOnce(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL3()
	lf.budget.state.FastBurning = true
	for i := 0; i < 3; i++ {
		lf.limit(freezeRequest(lf.clock.Now()))
	}
	lf.budget.state.FastBurning = false
	for i := 0; i < 2; i++ {
		lf.limit(freezeRequest(lf.clock.Now()))
	}
	// The ledger is per database (P0-5), so its promotion events name the
	// database too; keep the cap transitions.
	var evs []Event
	var types []string
	for _, e := range lf.events(EventFilter{Family: FamilyWraparound, Class: ClassFreeze,
		Database: "orders"}) {
		types = append(types, string(e.Type))
		if e.Type == EventCapped || e.Type == EventCapCleared {
			evs = append(evs, e)
		}
	}
	if len(evs) != 2 || evs[0].Type != EventCapCleared || evs[1].Type != EventCapped ||
		!strings.Contains(evs[1].Reason, DowngradeBudgetBurn) || evs[1].Database != "orders" {
		t.Fatalf("cap events = %v", types)
	}
}

func TestLimitBelowL2SkipsSignalQueries(t *testing.T) {
	lf := newLimiterFixture(t)
	got := lf.limit(freezeRequest(lf.clock.Now()))
	if got.Level != 1 || got.Downgraded || lf.budget.calls != 0 {
		t.Fatalf("default pair: %+v, budget calls %d", got, lf.budget.calls)
	}
}

func TestConcurrentLimitCalls(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL3()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := lf.lim.Limit(lf.ctx, freezeRequest(lf.clock.Now()))
			if err == nil && got.Level != 3 {
				err = errors.New("level changed under concurrency")
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestLimitStoreFailureIsAnError(t *testing.T) {
	lf := newLimiterFixture(t)
	ctx, cancel := context.WithCancel(lf.ctx)
	cancel()
	if _, err := lf.lim.Limit(ctx, freezeRequest(lf.clock.Now())); err == nil {
		t.Fatal("a ledger read failure must surface so the gate fails closed")
	}
}
