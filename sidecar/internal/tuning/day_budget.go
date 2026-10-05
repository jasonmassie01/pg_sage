package tuning

import (
	"context"
	"time"
)

// dayBudget is the cycle's view of the durable per-UTC-day budget
// (sage.tuning_budget_day): what was charged before the cycle and what the
// cycle has charged since.
type dayBudget struct {
	day                     time.Time
	used                    int64
	chargedTok, chargedReqs int64
}

// openDay reads the day's spend and returns the tokens this cycle may
// use, or false when the model must not be asked: the day's spend is
// unreadable, or less is left than one request's output reservation.
func (a *Agent) openDay(ctx context.Context, cy *cycle) (int64, bool) {
	cy.day = dayBudget{day: a.now().UTC()}
	used, _, err := a.deps.Store.DayBudgetUsed(ctx, cy.day.day)
	if err != nil {
		a.logFn("WARN", "tuning: the daily budget is unreadable, not asking the model "+
			"this cycle: %v", err)
		return 0, false
	}
	cy.day.used = used
	tokens := int64(a.settings.Tuning.MaxTokensPerCycle)
	limit := a.settings.DailyTokenLimit
	if limit <= 0 {
		return tokens, true
	}
	left := max(limit-used, 0)
	if left < int64(max(a.settings.MaxOutputTokens, 1)) {
		a.logFn("INFO", "tuning: the daily budget is spent (%d of %d tokens on %s UTC), "+
			"cases wait for the next UTC day", used, limit, utcDay(cy.day.day))
		return 0, false
	}
	return min(tokens, left), true
}

// chargeDay records the cycle's spend since the last charge.
func (a *Agent) chargeDay(ctx context.Context, cy *cycle) error {
	reqs, tokens := cy.budget.Used()
	dTok, dReqs := tokens-cy.day.chargedTok, int64(reqs)-cy.day.chargedReqs
	if dTok <= 0 && dReqs <= 0 {
		return nil
	}
	if err := a.deps.Store.ChargeDayBudget(ctx, cy.day.day, max(dTok, 0),
		max(dReqs, 0)); err != nil {
		return err
	}
	cy.day.chargedTok, cy.day.chargedReqs = tokens, int64(reqs)
	return nil
}

// deferAll leaves every case for a later cycle without asking the model.
func (a *Agent) deferAll(cy *cycle, cases []Case) {
	ids := make([]string, 0, len(cases))
	for _, c := range cases {
		ids = append(ids, c.ID)
	}
	a.queue.advance(ids)
	a.logDeferred(ids, true)
	a.noteCycle(cy, 0, len(ids))
}
