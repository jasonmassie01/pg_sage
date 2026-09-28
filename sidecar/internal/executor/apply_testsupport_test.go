package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// executeFinding runs one finding through Apply as an already authorized
// execute decision, so a test can pin the decision id and lock ceiling
// without a policy store. It exercises the production pipeline (lease and
// park, DDL slot, load admission, execution under the ceiling): only the
// gate's verdict is fixed. The D1 and D6 tests were written against the
// pre-Apply executeFinding, which took the lease itself.
func (e *Executor) executeFinding(
	ctx context.Context, f analyzer.Finding, findingID int64,
	decision ActionPolicyDecision,
) {
	decision.Decision = PolicyDecisionExecute
	intent := e.findingIntent(f, findingID, false)
	intent.Authorize = func(context.Context) (ActionPolicyDecision, error) {
		return decision, nil
	}
	_, _ = e.Apply(ctx, intent)
}
