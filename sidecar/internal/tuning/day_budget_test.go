package tuning

import (
	"context"
	"testing"
	"time"
)

// lifeos (v1.10.0): the old optimizer's daily token budget was an
// in-memory counter, so the 18:13 restart granted a fresh day of spend
// after it was exhausted at 16:06. The agent's daily budget is charged to
// sage.tuning_budget_day per UTC day, so a restart (a new agent on the
// same database) starts where the last one stopped.

// restartedAgent is a new agent on h's store: what a process restart sees.
func restartedAgent(h *harness, s Settings, now time.Time) *Agent {
	return New(s, Deps{Model: h.model, Indexes: h.indexes, Facts: h.facts, Hints: h.hints,
		Store: h.store, Now: func() time.Time { return now }}, h.logs.fn)
}

func dayUsed(h *harness, at time.Time) int64 {
	tokens, _, _ := h.store.DayBudgetUsed(context.Background(), at)
	return tokens
}

func tuneAgent(t *testing.T, a *Agent) {
	t.Helper()
	prev, cur := threeStatementPair()
	if _, err := a.Tune(context.Background(), cur, prev); err != nil {
		t.Fatalf("tune: %v", err)
	}
}

func TestTune_DailyBudgetSurvivesARestart(t *testing.T) {
	s := defaultSettings()
	s.DailyTokenLimit = 250 // each model call spends 100
	s.MaxOutputTokens = 100 // a request needs at least its output reservation
	h := newHarnessWith(t, s)
	tuneAgent(t, h.agent)
	if got := dayUsed(h, t0); got != 200 {
		t.Fatalf("charged %d tokens today, want the two calls that fit (200)", got)
	}
	calls := h.model.callCount()
	restarted := restartedAgent(h, s, t0.Add(time.Hour))
	tuneAgent(t, restarted)
	if got := dayUsed(h, t0); got != 200 {
		t.Fatalf("after a restart the day is still nearly spent: %d tokens", got)
	}
	st := restarted.Stats()
	if st.DayTokensUsed != 200 || st.DayTokenLimit != 250 || st.TokensUsed != 0 ||
		st.CasesDeferred != 3 {
		t.Fatalf("stats after restart = %+v", st)
	}
	if h.model.callCount() != calls {
		t.Fatalf("the restarted agent asked the model %d more times with 50 tokens left",
			h.model.callCount()-calls)
	}
	if !h.logs.contains("daily") {
		t.Fatal("the exhausted daily budget is logged")
	}
}

func TestTune_DailyBudgetIsAUTCDay(t *testing.T) {
	s := defaultSettings()
	s.DailyTokenLimit = 250
	h := newHarnessWith(t, s)
	h.store.day = map[string][2]int64{t0.UTC().Format(time.DateOnly): {250, 3}}
	next := t0.UTC().Truncate(24 * time.Hour).Add(24*time.Hour + time.Minute)
	tuneAgent(t, restartedAgent(h, s, next))
	if got := dayUsed(h, next); got == 0 {
		t.Fatal("a new UTC day has a fresh budget, whenever the process started")
	}
	if got := dayUsed(h, t0); got != 250 {
		t.Fatalf("the previous day is untouched: %d", got)
	}
}

func TestTune_DailyBudgetUnreadableAsksNothing(t *testing.T) {
	s := defaultSettings()
	s.DailyTokenLimit = 250
	h := newHarnessWith(t, s)
	h.store.dayErr = errFake
	tuneAgent(t, h.agent)
	if h.model.callCount() != 0 || !h.logs.contains("daily") {
		t.Fatalf("without the day's spend the model is not asked (calls %d)",
			h.model.callCount())
	}
	if h.agent.Stats().CasesDeferred != 3 {
		t.Fatalf("stats = %+v", h.agent.Stats())
	}
}

func TestTune_DailyBudgetChargeFailureStopsTheCycle(t *testing.T) {
	s := defaultSettings()
	s.DailyTokenLimit = 10_000
	h := newHarnessWith(t, s)
	h.store.chargeErr = errFake
	tuneAgent(t, h.agent)
	if h.model.callCount() != 1 {
		t.Fatalf("spend that cannot be recorded stops the cycle after one case: %d calls",
			h.model.callCount())
	}
}

func TestTune_NoDailyLimitStillRecordsTheSpend(t *testing.T) {
	h := newHarness(t)
	tuneAgent(t, h.agent)
	if got := dayUsed(h, t0); got != 300 {
		t.Fatalf("charged %d, want 300 (three calls)", got)
	}
}
