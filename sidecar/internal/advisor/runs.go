package advisor

import (
	"context"
	"sync"
	"time"
)

// firstFailureBackoff is how long a failed sub-advisor waits before its
// first retry; each further consecutive failure doubles it, capped at
// advisor.interval_seconds (advisors-02).
const firstFailureBackoff = time.Hour

// claimLease is how long a claimed run blocks a concurrent cycle (or a
// replica) from claiming the same sub-advisor. The run's outcome replaces
// it; it only outlives a run that crashed mid-call.
const claimLease = 15 * time.Minute

// subAdvisorNames are the sub-advisors whose schedule is persisted.
var subAdvisorNames = []string{"wal", "connections", "rewrites", "bloat"}

// runState is one sub-advisor's schedule (sage.advisor_runs). A run counts
// only on success; failures (model errors, token budget) back off.
type runState struct {
	LastSuccessAt       time.Time
	ConsecutiveFailures int
	NextAttemptAt       time.Time
}

// due reports whether the sub-advisor may run at now. The interval
// elapsing exactly counts as due.
func (s runState) due(now time.Time) bool { return !now.Before(s.NextAttemptAt) }

// succeeded is the state after a successful run at now.
func (s runState) succeeded(now time.Time, interval time.Duration) runState {
	return runState{LastSuccessAt: now, NextAttemptAt: now.Add(interval)}
}

// failed is the state after a failed run at now. The last success is kept.
func (s runState) failed(now time.Time, interval time.Duration) runState {
	s.ConsecutiveFailures++
	s.NextAttemptAt = now.Add(failureBackoff(s.ConsecutiveFailures, interval))
	return s
}

// failureBackoff is 1h after the first failure, doubling per consecutive
// failure, never longer than the advisor interval.
func failureBackoff(failures int, interval time.Duration) time.Duration {
	d := firstFailureBackoff
	for i := 1; i < failures && d < interval; i++ {
		d *= 2
	}
	return min(d, max(interval, 0))
}

// runStore persists sub-advisor schedules. ClaimRun atomically takes a due
// sub-advisor's run (false when it is not due or another cycle holds it).
type runStore interface {
	LoadRuns(ctx context.Context) (map[string]runState, error)
	ClaimRun(ctx context.Context, name string, now, leaseUntil time.Time) (bool, error)
	SaveRun(ctx context.Context, name string, st runState) error
}

// memRunStore is the schedule without a database (nil pool): it lasts as
// long as the process, with the same claim semantics.
type memRunStore struct {
	mu   sync.Mutex
	runs map[string]runState
}

func newMemRunStore() *memRunStore { return &memRunStore{runs: map[string]runState{}} }

func (m *memRunStore) LoadRuns(context.Context) (map[string]runState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]runState, len(m.runs))
	for k, v := range m.runs {
		out[k] = v
	}
	return out, nil
}

func (m *memRunStore) ClaimRun(_ context.Context, name string, now,
	leaseUntil time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.runs[name]
	if !st.due(now) {
		return false, nil
	}
	st.NextAttemptAt = leaseUntil
	m.runs[name] = st
	return true, nil
}

func (m *memRunStore) SaveRun(_ context.Context, name string, st runState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[name] = st
	return nil
}

// runSchedule is the advisor's in-memory mirror of the persisted schedule,
// loaded once on start (retried until a load succeeds).
type runSchedule struct {
	mu     sync.Mutex
	loaded bool
	runs   map[string]runState
}

func (s *runSchedule) get(name string) runState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[name]
}

func (s *runSchedule) set(name string, st runState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs == nil {
		s.runs = map[string]runState{}
	}
	s.runs[name] = st
}

func (s *runSchedule) replace(runs map[string]runState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs, s.loaded = runs, true
}

func (s *runSchedule) isLoaded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loaded
}

// ShouldRun reports whether any sub-advisor is due now.
func (a *Advisor) ShouldRun() bool {
	now := a.now()
	for _, name := range subAdvisorNames {
		if a.schedule.get(name).due(now) {
			return true
		}
	}
	return false
}

// loadSchedule reads the persisted schedule once. A failed load leaves
// the mirror empty: the claim, which reads the table, still decides.
func (a *Advisor) loadSchedule(ctx context.Context) {
	if a.schedule.isLoaded() {
		return
	}
	runs, err := a.runs.LoadRuns(ctx)
	if err != nil {
		a.logFn("WARN", "advisor: load sub-advisor schedule: %v", err)
		return
	}
	a.schedule.replace(runs)
}

// claim takes name's run when it is due. A store error skips it: a run
// that cannot be recorded must not spend model tokens (fail closed).
func (a *Advisor) claim(ctx context.Context, name string, now time.Time) bool {
	if !a.schedule.get(name).due(now) {
		return false
	}
	ok, err := a.runs.ClaimRun(ctx, name, now, now.Add(claimLease))
	if err != nil {
		a.logFn("WARN", "advisor: %s: claim run: %v; skipped this cycle", name, err)
		return false
	}
	if !ok {
		a.logFn("DEBUG", "advisor: %s: not due or claimed by another cycle", name)
		if runs, err := a.runs.LoadRuns(ctx); err == nil {
			a.schedule.replace(runs)
		}
	}
	return ok
}

// record saves a claimed run's outcome. A save error is logged; the
// lease keeps the sub-advisor from re-running until it expires.
func (a *Advisor) record(ctx context.Context, name string, now time.Time, runErr error) {
	interval := a.cfg.Advisor.Interval()
	st := a.schedule.get(name)
	if runErr != nil {
		st = st.failed(now, interval)
	} else {
		st = st.succeeded(now, interval)
	}
	a.schedule.set(name, st)
	if err := a.runs.SaveRun(ctx, name, st); err != nil {
		a.logFn("WARN", "advisor: %s: save run outcome: %v", name, err)
	}
}
