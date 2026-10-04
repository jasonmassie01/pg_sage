package executor

import (
	"context"

	"github.com/pg-sage/sidecar/internal/policy"
)

// WithFactBinder installs the database's confirmed facts (roadmap 2.3).
// The standing gate reads it on every authorization, so it applies however
// the gate and the binder are ordered; nil consults no facts.
func (e *Executor) WithFactBinder(binder policy.FactBinder) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.facts = binder
}

// executorFacts is the gate's view of the executor's current fact binder.
type executorFacts struct{ e *Executor }

// Bind implements policy.FactBinder.
func (f executorFacts) Bind(ctx context.Context, req policy.ActionRequest) (
	[]policy.FactBinding, error) {
	f.e.policyMu.RLock()
	binder := f.e.facts
	f.e.policyMu.RUnlock()
	if binder == nil {
		return nil, nil
	}
	return binder.Bind(ctx, req)
}
