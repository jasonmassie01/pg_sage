package budget

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/verify"
)

// WithCancelCounter gives the monitor the database's statement-timeout
// cancels from the server log (market-08b). Without one the cancels are
// unknown (cancel_source none) and an enforced timeout needs approval.
func (m *Monitor) WithCancelCounter(c *CancelCounter) *Monitor {
	m.cancels.Store(c)
	return m
}

// cancelSource is where this cycle's cancels come from.
func (m *Monitor) cancelSource() string {
	if m.cancels.Load().Active() {
		return CancelSourceLog
	}
	return CancelSourceNone
}

// recordCancels writes the cancels counted since the last cycle to each
// role budget's ledger row, in warn mode too, so the promotion days hold
// a pre-enforcement baseline. A failed write keeps them for the next
// cycle.
func (m *Monitor) recordCancels(ctx context.Context, today time.Time, budgets []Budget) error {
	if m.cancelLedger == nil {
		return nil
	}
	source := m.cancelSource()
	var batch CancelBatch
	if source == CancelSourceLog {
		batch = m.cancels.Load().Take()
	}
	rows := cancelRows(budgets, batch, today, source)
	if err := m.cancelLedger.RecordCancels(ctx, today, source, rows); err != nil {
		m.cancels.Load().Restore(batch)
		return fmt.Errorf("record statement-timeout cancels from the server log: %w", err)
	}
	return nil
}

type rowKey struct {
	budget string
	day    time.Time
}

// cancelRows spreads a batch over the role budgets matching each role: a
// row per budget and day with cancels or a lower bound, and today's row
// of every role budget, so its cancel_source is recorded. None without a
// log source.
func cancelRows(budgets []Budget, batch CancelBatch, today time.Time,
	source string) []CancelRow {
	if source != CancelSourceLog {
		return nil
	}
	lower := map[time.Time]bool{}
	for _, d := range batch.LowerBound {
		lower[utcDay(d)] = true
	}
	var out []CancelRow
	index := map[rowKey]int{}
	add := func(b Budget, day time.Time, n int64) {
		k := rowKey{budget: b.Name, day: utcDay(day)}
		if i, ok := index[k]; ok {
			out[i].Count += n
			return
		}
		index[k] = len(out)
		out = append(out, CancelRow{Budget: b.Name, Definition: b.Definition(), Day: k.day,
			Count: n, LowerBound: lower[k.day]})
	}
	for _, b := range budgets {
		if b.Match.Role == "" {
			continue
		}
		add(b, today, 0)
		for d := range lower {
			add(b, d, 0)
		}
		for _, rc := range batch.Counts {
			if rc.Role == b.Match.Role {
				add(b, rc.Day, rc.Count)
			}
		}
	}
	return out
}

// promotable is CanPromote after the budget's last revert: a reverted
// statement_timeout keeps the budget in warn mode for a fresh 7 days.
func (m *Monitor) promotable(ctx context.Context, b Budget, history []ShadowDay,
	now time.Time) (bool, string, error) {
	if m.cancelLedger == nil || b.StatementTimeoutMs <= 0 {
		ok, why := CanPromote(history, now)
		return ok, why, nil
	}
	db, err := m.catalog.CurrentDatabase(ctx)
	if err != nil {
		return false, "", fmt.Errorf("read the database name for budget %q: %w", b.Name, err)
	}
	sql := RoleSettingSQL(b.Match.Role, db, SettingStatementTimeout, b.StatementTimeoutMs)
	reverted, err := m.cancelLedger.LastRevert(ctx, sql)
	if err != nil {
		return false, "", fmt.Errorf("read the last revert of budget %q: %w", b.Name, err)
	}
	ok, why := CanPromoteSince(history, now, reverted)
	return ok, why, nil
}

// addCancelWatch gives a statement_timeout action what its verification
// needs: the budget's ledger key and its predicted cancels per hour
// (before_state.cancel_watch, verify.Prediction). Without a log source
// the revert cannot be verified, so the action needs approval.
func (m *Monitor) addCancelWatch(f *analyzer.Finding, b Budget, history []ShadowDay,
	now time.Time) {
	perHour, days := PredictedPerHour(history, now)
	source := m.cancelSource()
	f.Detail["cancel_watch"] = map[string]any{"budget": b.Name, "definition": b.Definition(),
		"ledger_database": m.database, "role": b.Match.Role, "predicted_per_hour": perHour,
		"prediction_days": days, "source": source}
	f.Detail["predicted_effect"] = map[string]any{"class": verify.ClassRoleTimeout,
		"method": verify.MethodRule, "metric": verify.MetricStatementTimeoutCancels,
		"expected_change_pct": 0.0, "baseline": perHour, "source": "budget",
		"note": fmt.Sprintf("%.1f statement-timeout cancels per hour expected (the %d "+
			"promotion days' would-cancels plus actual cancels); more than twice that "+
			"in the first 24 hours reverts it", perHour, days)}
	if source == CancelSourceLog {
		f.Recommendation += fmt.Sprintf(" Its cancels are counted from the server log: "+
			"more than twice the predicted %.1f per hour (at least 11) within 24 hours "+
			"reverts it automatically.", perHour)
		return
	}
	f.Detail[analyzer.DetailApprovalRequired] = NoLogApprovalReason
	f.Recommendation = NoLogApprovalReason + ": pg_sage reads no server log for this " +
		"database, so nothing counts the statements this timeout cancels and it cannot " +
		"be reverted automatically; it needs an operator's approval. " + f.Recommendation
}
