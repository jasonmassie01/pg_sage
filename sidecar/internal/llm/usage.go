package llm

import (
	"context"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// llm-04: the attributed usage accumulator. Every counted call adds to the
// key (UTC day, database, client role, purpose, model); the sidecar
// flushes the deltas to the control database every minute and on
// shutdown. A failed flush keeps the deltas for the next one and logs once
// per failure streak. Record never blocks on I/O, so no call ever fails
// because of the ledger.

// DefaultUsageMaxKeys bounds the accumulator; later keys go to
// PurposeOverflow.
const DefaultUsageMaxKeys = 10_000

// UsageKey names one ledger row.
type UsageKey struct {
	Day      time.Time // UTC midnight
	Database string    // "" is the process-wide clients
	Role     string    // general or optimizer
	Purpose  string
	Model    string
}

// UsageCounts are a key's deltas since the last flush. CostUSD is only
// meaningful when Priced: every call in it had an operator price.
// Estimated marks a split estimated by pg_sage, not reported by the
// provider.
type UsageCounts struct {
	PromptTokens, CompletionTokens, ReasoningTokens int64
	Calls, Failures                                 int64
	CostUSD                                         float64
	Priced, Estimated                               bool
}

// add merges d into c. A key is priced only while every call in it was:
// a partial sum would understate the spend.
func (c *UsageCounts) add(d UsageCounts) {
	c.Priced = d.Priced && (c.Calls == 0 || c.Priced)
	c.PromptTokens += d.PromptTokens
	c.CompletionTokens += d.CompletionTokens
	c.ReasoningTokens += d.ReasoningTokens
	c.Calls += d.Calls
	c.Failures += d.Failures
	c.CostUSD += d.CostUSD
	c.Estimated = c.Estimated || d.Estimated
}

// UsageSink persists one flush's deltas; an error keeps them pending.
type UsageSink func(ctx context.Context, deltas map[UsageKey]UsageCounts) error

// PriceFunc returns a model's operator price; false means unpriced.
type PriceFunc func(model string) (config.LLMPrice, bool)

// PricingFrom looks prices up in an llm.pricing map (nil or empty: none).
func PricingFrom(prices map[string]config.LLMPrice) PriceFunc {
	return func(model string) (config.LLMPrice, bool) {
		p, ok := prices[model]
		return p, ok && p.InputPerM != nil && p.OutputPerM != nil
	}
}

// UsageAccumulator holds the deltas between flushes. It is safe for
// concurrent use; a nil accumulator records nothing.
type UsageAccumulator struct {
	maxKeys int
	price   PriceFunc
	logFn   func(string, string, ...any)
	now     func() time.Time

	flushMu sync.Mutex // one flush at a time
	mu      sync.Mutex // guards pending and failing
	pending map[UsageKey]*UsageCounts
	failing bool
}

// NewUsageAccumulator returns an accumulator of at most maxKeys keys (<= 0
// means DefaultUsageMaxKeys) pricing calls with price (nil: none).
func NewUsageAccumulator(
	maxKeys int, price PriceFunc, logFn func(string, string, ...any),
) *UsageAccumulator {
	if maxKeys <= 0 {
		maxKeys = DefaultUsageMaxKeys
	}
	if price == nil {
		price = PricingFrom(nil)
	}
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	return &UsageAccumulator{maxKeys: maxKeys, price: price, logFn: logFn,
		now: time.Now, pending: map[UsageKey]*UsageCounts{}}
}

// Record adds one call of a client (database, role) to its key.
func (a *UsageAccumulator) Record(database, role string, o Outcome) {
	if a == nil {
		return
	}
	purpose := o.Purpose
	if !ValidPurpose(purpose) {
		purpose = PurposeUnlabeled
	}
	k := UsageKey{Day: utcDay(a.now()), Database: database, Role: role,
		Purpose: purpose, Model: o.Model}
	d := a.delta(o)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addLocked(k, d)
}

// addLocked adds d to k, or to k's overflow key once the map is full.
func (a *UsageAccumulator) addLocked(k UsageKey, d UsageCounts) {
	c := a.pending[k]
	if c == nil && len(a.pending) >= a.maxKeys {
		k.Purpose = PurposeOverflow
		c = a.pending[k]
	}
	if c == nil {
		c = &UsageCounts{}
		a.pending[k] = c
	}
	c.add(d)
}

// delta is one call's counts. Without a provider split, the estimate is
// split into the prompt estimate and the rest as completion. Tokens the
// provider charged beyond its split (some report thinking tokens only in
// the total) count as completion: the ledger never shows less than the
// budget charged.
func (a *UsageAccumulator) delta(o Outcome) UsageCounts {
	d := UsageCounts{Calls: 1, PromptTokens: int64(max(o.PromptTokens, 0)),
		CompletionTokens: int64(max(o.CompletionTokens, 0)),
		ReasoningTokens:  int64(max(o.ReasoningTokens, 0))}
	if o.Result != ResultOK {
		d.Failures = 1
	}
	if rest := int64(max(o.Tokens, 0)) - d.PromptTokens - d.CompletionTokens; rest > 0 {
		d.CompletionTokens += rest
	}
	if o.Estimated {
		prompt := min(max(o.EstimatedPromptTokens, 0), max(o.Tokens, 0))
		d.PromptTokens = int64(prompt)
		d.CompletionTokens = int64(max(o.Tokens, 0) - prompt)
		d.ReasoningTokens, d.Estimated = 0, true
	}
	if p, ok := a.price(o.Model); ok {
		d.CostUSD, d.Priced = callCost(p, d), true
	}
	return d
}

// callCost prices prompt at input, completion at output, and the reasoning
// share of completion at the reasoning price when one is set.
func callCost(p config.LLMPrice, d UsageCounts) float64 {
	reasoning := min(d.ReasoningTokens, d.CompletionTokens)
	reasoningPrice := *p.OutputPerM
	if p.ReasoningPerM != nil {
		reasoningPrice = *p.ReasoningPerM
	}
	return (float64(d.PromptTokens)**p.InputPerM +
		float64(d.CompletionTokens-reasoning)**p.OutputPerM +
		float64(reasoning)*reasoningPrice) / 1e6
}

// CostOf is the USD cost of one model's tokens at its operator price, and
// false when price does not price model (the cost is then unknown, never a
// guess).
func CostOf(price PriceFunc, model string, prompt, completion, reasoning int64) (float64,
	bool) {
	if price == nil {
		return 0, false
	}
	p, ok := price(model)
	if !ok {
		return 0, false
	}
	return callCost(p, UsageCounts{PromptTokens: prompt, CompletionTokens: completion,
		ReasoningTokens: reasoning}), true
}

// Pending copies the deltas not yet flushed.
func (a *UsageAccumulator) Pending() map[UsageKey]UsageCounts {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[UsageKey]UsageCounts, len(a.pending))
	for k, c := range a.pending {
		out[k] = *c
	}
	return out
}

// Flush hands the pending deltas to sink. On failure they are merged back
// for the next flush and the first failure of a streak is logged.
func (a *UsageAccumulator) Flush(ctx context.Context, sink UsageSink) error {
	if a == nil {
		return nil
	}
	a.flushMu.Lock()
	defer a.flushMu.Unlock()
	a.mu.Lock()
	taken := a.pending
	a.pending = map[UsageKey]*UsageCounts{}
	a.mu.Unlock()
	if len(taken) == 0 {
		return nil
	}
	deltas := make(map[UsageKey]UsageCounts, len(taken))
	for k, c := range taken {
		deltas[k] = *c
	}
	err := sink(ctx, deltas)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		for k, c := range deltas {
			a.addLocked(k, c)
		}
		if !a.failing {
			a.logFn("llm", "LLM usage ledger flush of %d keys failed: %v; the "+
				"deltas are kept and retried every minute", len(deltas), err)
		}
		a.failing = true
		return err
	}
	if a.failing {
		a.logFn("llm", "LLM usage ledger flush recovered: %d keys written", len(deltas))
	}
	a.failing = false
	return nil
}

func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
