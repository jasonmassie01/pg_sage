package policy

import (
	"context"
	"strings"
)

// BatchExplainer explains many requests against one snapshot of runtime
// state, policy document and usage, for readiness views that cover every
// action family at once.
type BatchExplainer interface {
	ExplainBatch(context.Context, []ActionRequest) []Decision
}

// ExplainBatch runs Explain for each request with the runtime and policy
// readers memoized for the call, and usage memoized per usage question
// (memoizeUsageByRequest); it records nothing. All requests share the
// first request's runtime (same database, same replica state).
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
	snapshot.config.Usage = memoizeUsageByRequest(gate.config.Usage)
	decisions := make([]Decision, len(requests))
	for i, request := range requests {
		decisions[i] = snapshot.Explain(ctx, request)
	}
	return decisions
}

// memoizeUsageByRequest caches usage per usage question for the batch: the
// budget kind, the targets and the statement. Each kind has its own budget,
// and the window counts the request's own table and rows rewritten, so one
// table's usage is never handed to another (cases-04). Family requests (no
// targets, no SQL) share one read per kind. A nil reader stays nil.
func memoizeUsageByRequest(
	read func(context.Context, ActionRequest) (LimitUsage, error),
) func(context.Context, ActionRequest) (LimitUsage, error) {
	if read == nil {
		return nil
	}
	readers := map[string]func(context.Context, ActionRequest) (LimitUsage, error){}
	return func(ctx context.Context, req ActionRequest) (LimitUsage, error) {
		key := usageQuestion(req)
		if readers[key] == nil {
			readers[key] = memoize(read)
		}
		return readers[key](ctx, req)
	}
}

// usageQuestion identifies what a usage read answers. NUL cannot occur in
// an identifier or a statement the gate accepts, so it separates the parts.
func usageQuestion(req ActionRequest) string {
	return string(BudgetKindFor(req)) + "\x00" + strings.Join(req.TargetObjs, "\x00") +
		"\x00\x00" + req.SQL
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
