package policy

import "context"

// BatchExplainer explains many requests against one snapshot of runtime
// state, policy document and usage, for readiness views that cover every
// action family at once.
type BatchExplainer interface {
	ExplainBatch(context.Context, []ActionRequest) []Decision
}

// ExplainBatch runs Explain for each request with the runtime, policy and
// usage readers memoized for the call, and records nothing. All requests
// share the first request's runtime (same database, same replica state).
func (gate *authorizationGate) ExplainBatch(
	ctx context.Context, requests []ActionRequest,
) []Decision {
	if len(requests) == 0 {
		return nil
	}
	snapshot := &authorizationGate{config: gate.config}
	snapshot.config.RecordDecision = nil
	snapshot.config.RecordDecisionDetailed = nil
	snapshot.config.Runtime = memoize(gate.config.Runtime)
	snapshot.config.Policy = memoize(gate.config.Policy)
	snapshot.config.Usage = memoize(gate.config.Usage)
	decisions := make([]Decision, len(requests))
	for i, request := range requests {
		decisions[i] = snapshot.Explain(ctx, request)
	}
	return decisions
}

// memoize caches the first result (and error) of read for the batch. A nil
// reader stays nil so the gate's own missing-reader handling applies.
func memoize[T any](
	read func(context.Context, ActionRequest) (T, error),
) func(context.Context, ActionRequest) (T, error) {
	if read == nil {
		return nil
	}
	var (
		done   bool
		value  T
		result error
	)
	return func(ctx context.Context, req ActionRequest) (T, error) {
		if !done {
			value, result = read(ctx, req)
			done = true
		}
		return value, result
	}
}
