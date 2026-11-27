package rehearsal

import (
	"fmt"
	"time"

	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
)

const (
	// DefaultPlanFlipPct is rehearsal.plan_flip_pct's default.
	DefaultPlanFlipPct = 5.0
	// DefaultStepStatementTimeout is the production applier's
	// statement_timeout when Options.StatementTimeout is not set.
	DefaultStepStatementTimeout = 10 * time.Minute
	// DefaultMaxLock is migration.max_lock_ms's default.
	DefaultMaxLock = 2 * time.Second
	// stepBudgetFactor is the margin a rehearsed step's time gets before
	// it is compared with the statement timeout: production is slower
	// than a quiet clone.
	stepBudgetFactor    = 1.5
	lockAccessExclusive = "ACCESS EXCLUSIVE"
)

// StepMeasurement is one rehearsed step: its wall time, whether it ran
// CONCURRENTLY (top level), and the table lock it takes.
type StepMeasurement struct {
	Kind       planpkg.StepKind
	Duration   time.Duration
	Concurrent bool
	LockLevel  string
}

func (o *Orchestrator) planFlipPct() float64 {
	if o.options.PlanFlipPct > 0 {
		return o.options.PlanFlipPct
	}
	return DefaultPlanFlipPct
}

func (o *Orchestrator) statementTimeout() time.Duration {
	if o.options.StatementTimeout > 0 {
		return o.options.StatementTimeout
	}
	return DefaultStepStatementTimeout
}

func (o *Orchestrator) maxLock() time.Duration {
	if o.options.MaxLock > 0 {
		return o.options.MaxLock
	}
	return DefaultMaxLock
}

// overBudget reports the first step that would not fit production: 1.5x
// its time exceeds the statement timeout, or, without CONCURRENTLY, it
// held ACCESS EXCLUSIVE (or a lock nobody stated) past max_lock.
func (o *Orchestrator) overBudget(measurement Measurement) (string, bool) {
	timeout, maxLock := o.statementTimeout(), o.maxLock()
	for _, step := range measurement.Steps {
		scaled := time.Duration(float64(step.Duration) * stepBudgetFactor)
		if scaled > timeout {
			return fmt.Sprintf("step %s ran %s on the clone; x%.1f = %s exceeds the "+
				"%s statement timeout", step.Kind, step.Duration, stepBudgetFactor,
				scaled, timeout), true
		}
		exclusive := step.LockLevel == lockAccessExclusive || step.LockLevel == ""
		if !step.Concurrent && exclusive && step.Duration > maxLock {
			return fmt.Sprintf("step %s held ACCESS EXCLUSIVE for %s on the clone, over "+
				"migration.max_lock_ms (%d)", step.Kind, step.Duration,
				maxLock.Milliseconds()), true
		}
	}
	return "", false
}
