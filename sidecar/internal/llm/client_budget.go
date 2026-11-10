package llm

import (
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// budgetReservation is one admitted request's holds, released on failure
// and reconciled to actual usage on success.
type budgetReservation struct {
	tokens   int
	external int
	// epoch is the external budget's reset epoch when its hold was taken
	// (HoldBudgeter only), so a reset in flight is not subtracted twice.
	epoch uint64
	// global is the process-wide budget the hold was charged to, or nil.
	global *Budget
}

// HoldBudgeter is a Budgeter that can be reset while a call is in flight
// (the fleet budget's UTC midnight and manual resets). Without it a reset
// discards the hold, and settling actual-hold under-counts the new day.
type HoldBudgeter interface {
	Budgeter
	// Hold charges tokens when they fit and returns the reset epoch it was
	// charged in.
	Hold(tokens int) (epoch uint64, ok bool)
	// Settle replaces a hold from epoch with actual usage: actual-held while
	// the epoch is current, the full actual after a reset.
	Settle(held, actual int, epoch uint64)
}

type ceilingFunc func() (int64, bool)

// SetCeiling installs a stricter daily cap read on every admission, such
// as the standing policy's budgets.llm_tokens_daily. It returns the cap
// and true, or false for no cap; nil removes it. The effective limit is
// the stricter of llm.token_budget_daily and the ceiling.
func (c *Client) SetCeiling(ceiling func() (int64, bool)) {
	if ceiling == nil {
		c.ceiling.Store(nil)
		return
	}
	f := ceilingFunc(ceiling)
	c.ceiling.Store(&f)
}

// SetGlobalBudget charges every request of this client to a process-wide
// budget as well. nil detaches it.
func (c *Client) SetGlobalBudget(b *Budget) { c.global.Store(b) }

// dailyCap is the effective daily limit; limited=false means unlimited.
type dailyCap struct {
	limit   int64
	limited bool
	policy  bool // the ceiling, not the config, sets the limit
}

// dailyLimit reads the ceiling, which may consult the policy store, so
// callers must not hold budgetMu.
func (c *Client) dailyLimit(cfg config.LLMConfig) dailyCap {
	var out dailyCap
	if cfg.TokenBudgetDaily > 0 {
		out = dailyCap{limit: int64(cfg.TokenBudgetDaily), limited: true}
	}
	f := c.ceiling.Load()
	if f == nil {
		return out
	}
	value, ok := (*f)()
	if !ok || value < 0 {
		return out
	}
	if !out.limited || value < out.limit {
		return dailyCap{limit: value, limited: true, policy: true}
	}
	return out
}

// EffectiveTokenBudgetDaily returns the daily cap in force: the stricter
// of llm.token_budget_daily and the ceiling, 0 when unlimited. A policy
// cap of 0 also reads 0; IsBudgetExhausted then reports true.
func (c *Client) EffectiveTokenBudgetDaily() int {
	cfg, _ := c.configSnapshot()
	return int(c.dailyLimit(cfg).limit)
}

// rolloverLocked starts a new UTC day. Spend restarts at zero; holds of
// calls still in flight stay, so their settle cannot drive the hold
// negative and over-admit. Caller holds budgetMu.
func (c *Client) rolloverLocked(now time.Time) {
	today := budgetDay(now)
	if today != c.budgetResetDay.Load() {
		c.tokensUsedToday.Store(0)
		c.budgetResetDay.Store(today)
	}
}

func (c *Client) checkDailyLocked(capacity dailyCap, tokens int) error {
	if !capacity.limited {
		return nil
	}
	suffix := ""
	if capacity.policy {
		suffix = " by policy budgets.llm_tokens_daily"
	}
	used := c.tokensUsedToday.Load()
	if used >= capacity.limit {
		return budgetError(fmt.Sprintf("daily token budget exhausted (%d/%d)%s",
			used, capacity.limit, suffix))
	}
	if used+c.reservedTokens+int64(tokens) > capacity.limit {
		return budgetError(fmt.Sprintf(
			"daily token budget exhausted (%d reserved or used/%d)%s",
			used+c.reservedTokens, capacity.limit, suffix))
	}
	return nil
}

// reserveBudget admits a request against the client's daily cap, then the
// global budget, then the external (per-database) budget. Lock order is
// client, then global.
func (c *Client) reserveBudget(
	cfg config.LLMConfig, tokens int, external int,
) (budgetReservation, error) {
	capacity := c.dailyLimit(cfg)
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	c.rolloverLocked(time.Now())
	if err := c.checkDailyLocked(capacity, tokens); err != nil {
		return budgetReservation{}, err
	}
	reservation := budgetReservation{tokens: tokens}
	if global := c.global.Load(); global != nil {
		if err := global.hold(int64(tokens)); err != nil {
			return budgetReservation{}, err
		}
		reservation.global = global
	}
	if err := c.holdExternal(&reservation, external); err != nil {
		reservation.global.settle(int64(tokens), 0)
		return budgetReservation{}, err
	}
	c.reservedTokens += int64(tokens)
	return reservation, nil
}

// holdExternal charges the estimate to the external budget before
// provider I/O. A failed request releases it; a completed one reconciles
// it to actual use.
func (c *Client) holdExternal(r *budgetReservation, external int) error {
	if c.budget == nil {
		return nil
	}
	if held, ok := c.budget.(HoldBudgeter); ok {
		epoch, fits := held.Hold(external)
		if !fits {
			return budgetError("per-database token budget exhausted")
		}
		r.external, r.epoch = external, epoch
		return nil
	}
	externalBudgetMu.Lock()
	defer externalBudgetMu.Unlock()
	if !c.budget.CanSpend(external) {
		return budgetError("per-database token budget exhausted")
	}
	c.budget.Spend(external)
	r.external = external
	return nil
}

func (c *Client) settleExternal(r budgetReservation, actual int) {
	if c.budget == nil {
		return
	}
	if held, ok := c.budget.(HoldBudgeter); ok {
		held.Settle(r.external, actual, r.epoch)
		return
	}
	if r.external == 0 && actual == 0 {
		return
	}
	externalBudgetMu.Lock()
	c.budget.Spend(actual - r.external)
	externalBudgetMu.Unlock()
}

func (c *Client) releaseBudget(reservation budgetReservation) {
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	c.reservedTokens -= int64(reservation.tokens)
	reservation.global.settle(int64(reservation.tokens), 0)
	c.settleExternal(reservation, 0)
}

func (c *Client) reconcileBudget(reservation budgetReservation, actual int) {
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	c.rolloverLocked(time.Now())
	c.reservedTokens -= int64(reservation.tokens)
	c.tokensUsedToday.Add(int64(actual))
	reservation.global.settle(int64(reservation.tokens), int64(actual))
	c.settleExternal(reservation, actual)
}
