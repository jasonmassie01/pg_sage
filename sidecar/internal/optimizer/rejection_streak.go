package optimizer

import "context"

// Model skipping (owner decision, W2-C follow-up): when a table's last
// SkipLLMAfter proposals were all wasted — every candidate of the reply a
// memory hit or a fresh what-if rejection — and nothing changed materially
// since the streak began, the model is not asked about the table until a
// material change or the max age. A reply with no candidates (or one that
// failed) neither extends nor breaks the streak; any other outcome (an
// admitted candidate, an unverified one, a validator rejection) breaks it.
// Streaks live in memory: after a restart a table may be asked
// SkipLLMAfter more times before it is skipped again.

// tableStreak is a table's run of wasted proposals; baseline holds the
// workload and row estimate when it began (MeasuredAt = start), compared
// with the same material-change rules as a remembered rejection.
type tableStreak struct {
	wasted   int
	baseline rejection
}

// skipModel reports whether the table's streak says not to ask the model.
// A streak whose workload changed materially (or aged out) is dropped.
func (m *rejectionMemory) skipModel(tc TableContext) bool {
	if m == nil {
		return false
	}
	key := tc.Schema + "." + tc.Table
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.streaks[key]
	if s == nil || s.wasted < m.settings.SkipLLMAfter {
		return false
	}
	if materialChange(s.baseline, tc, m.settings, m.now()) != "" {
		delete(m.streaks, key)
		return false
	}
	return true
}

// noteProposal records one model reply for the table: candidates proposed,
// of which wasted were memory hits or what-if rejections.
func (m *rejectionMemory) noteProposal(tc TableContext, candidates, wasted int) {
	if m == nil || candidates == 0 {
		return
	}
	key := tc.Schema + "." + tc.Table
	m.mu.Lock()
	defer m.mu.Unlock()
	if wasted < candidates {
		delete(m.streaks, key)
		return
	}
	now := m.now()
	s := m.streaks[key]
	if s == nil || materialChange(s.baseline, tc, m.settings, now) != "" {
		s = &tableStreak{baseline: rejection{Workload: workloadOf(tc),
			RowEstimate: tc.LiveTuples, MeasuredAt: now}}
		m.streaks[key] = s
	}
	s.wasted++
}

type operatorRequestKey struct{}

// WithOperatorRequest marks an optimizer run an operator asked for: it
// always asks the model and measures every candidate, whatever rejection
// memory says (memory still records what it measures).
func WithOperatorRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, operatorRequestKey{}, true)
}

func operatorRequested(ctx context.Context) bool {
	requested, _ := ctx.Value(operatorRequestKey{}).(bool)
	return requested
}
