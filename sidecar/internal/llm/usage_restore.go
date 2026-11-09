package llm

import (
	"fmt"
	"time"
)

// Usage restore (fleet-10): the sidecar persists today's spend to the
// control database and adds it back at startup, so a restart mid-day does
// not hand out the full daily budget again. Each restore is called once
// per start with the usage an earlier process recorded; usage from
// another UTC day is ignored.

func dayStart(day int64) time.Time { return time.Unix(day*86400, 0).UTC() }

func validRestore(tokens int64) error {
	if tokens < 0 {
		return fmt.Errorf("LLM usage restore: negative token count %d", tokens)
	}
	return nil
}

// RestoreUsage adds tokens recorded on day to today's spend. It reports
// whether they were applied: false for another day or zero tokens.
func (c *Client) RestoreUsage(day time.Time, tokens int64) (bool, error) {
	if err := validRestore(tokens); err != nil {
		return false, err
	}
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	c.rolloverLocked(time.Now())
	if tokens == 0 || budgetDay(day) != c.budgetResetDay.Load() {
		return false, nil
	}
	c.tokensUsedToday.Add(tokens)
	return true, nil
}

// UsageSnapshot returns the UTC day the client's spend belongs to and the
// spend, read together. A client idle since midnight still reports the
// previous day until its next call rolls it over.
func (c *Client) UsageSnapshot() (time.Time, int64) {
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	day := c.budgetResetDay.Load()
	if day == 0 {
		day = budgetDay(time.Now())
	}
	return dayStart(day), c.tokensUsedToday.Load()
}

// RestoreUsage adds tokens recorded on day to the global budget's spend;
// a nil budget applies nothing.
func (b *Budget) RestoreUsage(day time.Time, tokens int64) (bool, error) {
	if err := validRestore(tokens); err != nil {
		return false, err
	}
	if b == nil {
		return false, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rolloverLocked()
	if tokens == 0 || budgetDay(day) != b.day {
		return false, nil
	}
	b.used += tokens
	return true, nil
}

// Usage returns the UTC day the global spend belongs to and the spend.
func (b *Budget) Usage() (time.Time, int64) {
	if b == nil {
		return dayStart(budgetDay(time.Now())), 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return dayStart(b.day), b.used
}
