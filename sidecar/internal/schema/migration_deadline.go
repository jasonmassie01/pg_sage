package schema

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// budgetExpired is the cause a bootstrap budget's context carries (ci-15),
// so a step that ends on it names the budget rather than the caller.
type budgetExpired struct {
	what   string
	budget time.Duration
}

func (b budgetExpired) Error() string {
	return fmt.Sprintf("the %s budget of %s", b.what, b.budget)
}

// listBudget is the budget of a list of steps (migrationBudget).
func listBudget(steps int) budgetExpired {
	return budgetExpired{what: "migration list", budget: migrationBudget(steps)}
}

// withBudget is context.WithTimeout carrying b as the cause.
func withBudget(ctx context.Context, b budgetExpired) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, b.budget, b)
}

// namedDeadline adds to a step's timeout which deadline ended it: the
// step's own budget (ctx still alive), a bootstrap budget, or the context
// the bootstrap was called with. Any other error is returned unchanged.
func namedDeadline(ctx context.Context, err error) error {
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if ctx.Err() == nil {
		return fmt.Errorf("%w (deadline: the step budget of %s expired)",
			err, migrationStepTimeout)
	}
	cause := context.Cause(ctx)
	var b budgetExpired
	switch {
	case errors.As(cause, &b):
		return fmt.Errorf("%w (deadline: %v expired)", err, b)
	case errors.Is(cause, context.DeadlineExceeded):
		return fmt.Errorf("%w (deadline: the context the bootstrap was called with "+
			"expired, not a bootstrap budget)", err)
	default:
		return err
	}
}
