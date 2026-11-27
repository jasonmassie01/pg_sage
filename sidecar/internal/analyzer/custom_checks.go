package analyzer

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// procInfo is one pg_proc row a check's called name resolved to.
type procInfo struct {
	Name       string
	Volatility string // provolatile: i, s or v
}

// checkState is one owner check's memory on this database. It is only
// touched by the analyzer's cycle goroutine.
type checkState struct {
	lastRun time.Time
	// verdict is a permanent refusal (a volatile or denied function).
	verdict error
	// allowed is set once pg_proc resolution accepted the check.
	allowed bool
	logged  bool
}

// customCheckRunner runs the owner-defined read-only checks of one
// database (market-11) on that database's pool.
type customCheckRunner struct {
	checks []config.PreparedCheck
	states map[string]*checkState
	// procLookup resolves called names in pg_proc; tests replace it.
	procLookup func(ctx context.Context, names []string) ([]procInfo, error)
	now        func() time.Time
	logFn      func(string, string, ...any)
}

func newCustomCheckRunner(
	checks []config.PreparedCheck, pool *pgxpool.Pool, logFn func(string, string, ...any),
) *customCheckRunner {
	r := &customCheckRunner{checks: checks, states: map[string]*checkState{},
		now: time.Now, logFn: logFn}
	r.procLookup = func(ctx context.Context, names []string) ([]procInfo, error) {
		return lookupProcs(ctx, pool, names)
	}
	return r
}

// WithCustomChecks attaches the prepared owner checks; none runs nothing.
func (a *Analyzer) WithCustomChecks(checks []config.PreparedCheck) {
	if len(checks) == 0 {
		a.checks = nil
		return
	}
	a.checks = newCustomCheckRunner(checks, a.pool, a.logFn)
}

func (r *customCheckRunner) state(id string) *checkState {
	st, ok := r.states[id]
	if !ok {
		st = &checkState{}
		r.states[id] = st
	}
	return st
}

// due reports whether the check runs on database now: its glob matches
// and its interval has passed since the last run.
func (r *customCheckRunner) due(p config.PreparedCheck, database string, now time.Time) bool {
	if !p.AppliesTo(database) {
		return false
	}
	last := r.state(p.ID).lastRun
	return last.IsZero() || now.Sub(last) >= time.Duration(p.IntervalMinutes)*time.Minute
}

// runCustomChecks runs the checks that are due. A check that is not due
// emits nothing and marks nothing, so its open findings stay as they are.
func (a *Analyzer) runCustomChecks(ctx context.Context) []Finding {
	if a.checks == nil {
		return nil
	}
	var out []Finding
	now := a.checks.now()
	for _, p := range a.checks.checks {
		if a.checks.due(p, a.databaseName, now) {
			out = append(out, a.runCustomCheck(ctx, p, now)...)
		}
	}
	return out
}

// runCustomCheck runs one check. A refusal, an error or a truncated
// result marks its category failed: its open findings stay open.
func (a *Analyzer) runCustomCheck(
	ctx context.Context, p config.PreparedCheck, now time.Time,
) []Finding {
	cat := p.Category()
	st := a.checks.state(p.ID)
	st.lastRun = now
	if err := a.checks.allow(ctx, p, st); err != nil {
		a.eval.fail(cat)
		if !st.logged {
			st.logged = true
			a.logFn("WARN", "analyzer: custom check %s is blocked: %v", p.ID, err)
		}
		return nil
	}
	cols, rows, err := a.execCustomCheck(ctx, p)
	if err != nil {
		a.eval.fail(cat)
		a.logFn("WARN", "analyzer: custom check %s failed: %v", p.ID, err)
		return nil
	}
	found, truncated, err := checkFindings(p, cols, rows)
	if err != nil {
		a.eval.fail(cat)
		a.logFn("WARN", "analyzer: custom check %s: %v", p.ID, err)
		return nil
	}
	if truncated {
		a.eval.fail(cat) // the unread rows may still hold open findings
		a.logFn("WARN", "analyzer: custom check %s returned more than %d rows; "+
			"open findings beyond them stay open", p.ID, p.MaxRows)
	}
	a.eval.evaluated(cat)
	return found
}

// allow resolves the check's called names in pg_proc on its first run. A
// lookup error blocks this run only; a refusal blocks the check for good.
func (r *customCheckRunner) allow(ctx context.Context, p config.PreparedCheck,
	st *checkState) error {
	if st.verdict != nil {
		return st.verdict
	}
	if st.allowed || len(p.Functions) == 0 {
		st.allowed = true
		return nil
	}
	procs, err := r.procLookup(ctx, p.Functions)
	if err != nil {
		return fmt.Errorf("resolve its functions in pg_proc: %w", err)
	}
	if err := checkVerdict(p, procs); err != nil {
		st.verdict = err
		return err
	}
	st.allowed = true
	return nil
}

// volatileAllowed are the volatile functions a check may call.
var volatileAllowed = map[string]bool{"now": true, "clock_timestamp": true}

// checkVerdict refuses a check that calls a denied function, or a
// volatile one outside the allowlist (any overload of the name counts).
func checkVerdict(p config.PreparedCheck, procs []procInfo) error {
	for _, name := range p.Functions {
		if config.DeniedCheckFunction(name) {
			return fmt.Errorf("function %s is not allowed in a check", name)
		}
	}
	for _, proc := range procs {
		if config.DeniedCheckFunction(proc.Name) {
			return fmt.Errorf("function %s is not allowed in a check", proc.Name)
		}
		if proc.Volatility == "v" && !volatileAllowed[proc.Name] {
			return fmt.Errorf("function %s is volatile (provolatile = 'v'); only "+
				"now and clock_timestamp are allowed", proc.Name)
		}
	}
	return nil
}
