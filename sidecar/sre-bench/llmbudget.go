package srebench

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Hard caps on a live model run (roadmap 2.4): requests, tokens, wall
// time and a spend estimate from the configured prices. The model tap
// admits each call before it leaves the process, reserving its estimate
// (prompt bytes / 4 plus the request's completion cap), and settles it
// with the provider's reported usage. A call that would pass a cap is
// refused, and so is every call after it: the run fails closed and its
// report says why.

// ErrBudgetExhausted is returned for a call past a cap.
var ErrBudgetExhausted = errors.New("live model budget exhausted")

// defaultMaxOutput is the completion estimate of a request that names no
// completion cap.
const defaultMaxOutput = 4096

// BudgetCaps are a live run's hard caps and the prices its spend
// estimate uses (US dollars per million tokens; reasoning tokens are
// billed as output).
type BudgetCaps struct {
	MaxRequests      int
	MaxTokens        int64
	MaxWall          time.Duration
	MaxSpendUSD      float64
	InputUSDPerMTok  float64
	OutputUSDPerMTok float64
}

func (c BudgetCaps) cost(prompt, output int64) float64 {
	return (float64(prompt)*c.InputUSDPerMTok + float64(output)*c.OutputUSDPerMTok) / 1e6
}

// Budget tracks one run's spending against its caps. A nil budget admits
// everything.
type Budget struct {
	caps  BudgetCaps
	now   func() time.Time
	start time.Time

	mu                     sync.Mutex
	requests               int
	tokens, reservedTokens int64
	spend, reservedSpend   float64
	exhausted              string
}

// Reservation is one admitted call's estimate.
type Reservation struct {
	tokens int64
	spend  float64
	live   bool
}

// NewBudget starts a budget's wall clock now.
func NewBudget(c BudgetCaps, now func() time.Time) *Budget {
	return &Budget{caps: c, now: now, start: now()}
}

// Admit reserves one call of promptBytes with a completion cap of
// maxOutput tokens, or refuses it (ErrBudgetExhausted) and every call
// after it.
func (b *Budget) Admit(promptBytes, maxOutput int) (Reservation, error) {
	if b == nil {
		return Reservation{}, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exhausted != "" {
		return Reservation{}, fmt.Errorf("%w: %s", ErrBudgetExhausted, b.exhausted)
	}
	estPrompt := int64((promptBytes + 3) / 4)
	est := estPrompt + int64(maxOutput)
	spend := b.caps.cost(estPrompt, int64(maxOutput))
	c := b.caps
	switch elapsed := b.now().Sub(b.start); {
	case elapsed >= c.MaxWall:
		b.exhausted = fmt.Sprintf("wall time %s reached the %s cap", elapsed.Round(time.Second),
			c.MaxWall)
	case b.requests+1 > c.MaxRequests:
		b.exhausted = fmt.Sprintf("requests: %d made, the cap is %d", b.requests,
			c.MaxRequests)
	case b.tokens+b.reservedTokens+est > c.MaxTokens:
		b.exhausted = fmt.Sprintf("tokens: %d used and %d reserved; a call estimated at "+
			"%d would pass the %d cap", b.tokens, b.reservedTokens, est, c.MaxTokens)
	case b.spend+b.reservedSpend+spend > c.MaxSpendUSD:
		b.exhausted = fmt.Sprintf("spend: $%.4f estimated; a call estimated at $%.4f would "+
			"pass the $%.2f cap", b.spend+b.reservedSpend, spend, c.MaxSpendUSD)
	default:
		b.requests++
		b.reservedTokens += est
		b.reservedSpend += spend
		return Reservation{tokens: est, spend: spend, live: true}, nil
	}
	return Reservation{}, fmt.Errorf("%w: %s", ErrBudgetExhausted, b.exhausted)
}

// Settle replaces an admitted call's estimate with its reported usage. A
// successful reply without usage keeps the estimate (fail closed); a
// failed reply without usage costs nothing. Usage past a cap exhausts
// the budget.
func (b *Budget) Settle(r Reservation, status int, u TapUsage) {
	if b == nil || !r.live {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reservedTokens -= r.tokens
	b.reservedSpend -= r.spend
	output := int64(u.CompletionTokens + u.ReasoningTokens)
	used, cost := int64(u.PromptTokens)+output, b.caps.cost(int64(u.PromptTokens), output)
	if used == 0 && status < 300 {
		used, cost = r.tokens, r.spend
	}
	b.tokens += used
	b.spend += cost
	switch {
	case b.exhausted != "":
	case b.tokens > b.caps.MaxTokens:
		b.exhausted = fmt.Sprintf("tokens: %d used, past the %d cap", b.tokens,
			b.caps.MaxTokens)
	case b.spend > b.caps.MaxSpendUSD:
		b.exhausted = fmt.Sprintf("spend: $%.4f estimated, past the $%.2f cap", b.spend,
			b.caps.MaxSpendUSD)
	}
}

// BudgetCapsRecord is the caps as the report shows them.
type BudgetCapsRecord struct {
	MaxRequests      int     `json:"max_requests"`
	MaxTokens        int64   `json:"max_tokens"`
	MaxWallSeconds   float64 `json:"max_wall_seconds"`
	MaxSpendUSD      float64 `json:"max_spend_usd"`
	InputUSDPerMTok  float64 `json:"input_usd_per_mtok"`
	OutputUSDPerMTok float64 `json:"output_usd_per_mtok"`
}

// BudgetRecord is a live run's spending as the report shows it.
type BudgetRecord struct {
	Requests    int              `json:"requests"`
	Tokens      int64            `json:"tokens"`
	SpendUSD    float64          `json:"spend_usd_estimate"`
	WallSeconds float64          `json:"wall_seconds"`
	Exhausted   bool             `json:"exhausted"`
	Reason      string           `json:"reason,omitempty"`
	Caps        BudgetCapsRecord `json:"caps"`
}

// Record is the budget so far; zero for a nil budget.
func (b *Budget) Record() BudgetRecord {
	if b == nil {
		return BudgetRecord{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.caps
	return BudgetRecord{Requests: b.requests, Tokens: b.tokens, SpendUSD: b.spend,
		WallSeconds: b.now().Sub(b.start).Seconds(), Exhausted: b.exhausted != "",
		Reason: b.exhausted, Caps: BudgetCapsRecord{MaxRequests: c.MaxRequests,
			MaxTokens: c.MaxTokens, MaxWallSeconds: c.MaxWall.Seconds(),
			MaxSpendUSD: c.MaxSpendUSD, InputUSDPerMTok: c.InputUSDPerMTok,
			OutputUSDPerMTok: c.OutputUSDPerMTok}}
}

// maxOutputOf is a chat request's completion cap (max_completion_tokens,
// else max_tokens), or defaultMaxOutput when it names none.
func maxOutputOf(body []byte) int {
	var req struct {
		MaxTokens     *int64 `json:"max_tokens"`
		MaxCompletion *int64 `json:"max_completion_tokens"`
	}
	if json.Unmarshal(body, &req) != nil {
		return defaultMaxOutput
	}
	for _, v := range []*int64{req.MaxCompletion, req.MaxTokens} {
		if v != nil && *v > 0 && *v <= 1<<24 {
			return int(*v)
		}
	}
	return defaultMaxOutput
}
