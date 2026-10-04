package tuning

import "sync"

// CycleBudget is one database's model budget for one analyzer cycle
// (owner decision 5): a request cap the loop takes from before each
// model call, and a token cap the LLM client charges (llm.Budgeter)
// before provider I/O. A nil or zero budget allows nothing.
type CycleBudget struct {
	mu          sync.Mutex
	maxRequests int
	maxTokens   int64
	requests    int
	tokens      int64
}

// NewCycleBudget returns a budget of maxRequests requests and maxTokens
// tokens; a negative limit allows nothing.
func NewCycleBudget(maxRequests int, maxTokens int64) *CycleBudget {
	return &CycleBudget{maxRequests: max(maxRequests, 0), maxTokens: max(maxTokens, 0)}
}

// TakeRequest takes one request; false when the cap is reached (a refusal
// takes nothing).
func (b *CycleBudget) TakeRequest() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.requests >= b.maxRequests {
		return false
	}
	b.requests++
	return true
}

// CanSpend reports whether tokens more fit under the cap.
func (b *CycleBudget) CanSpend(tokens int) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokens+int64(tokens) <= b.maxTokens
}

// Spend applies a usage delta; a negative delta releases a reservation and
// never takes the count below zero.
func (b *CycleBudget) Spend(tokens int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = max(b.tokens+int64(tokens), 0)
}

// Used returns the requests taken and tokens spent.
func (b *CycleBudget) Used() (int, int64) {
	if b == nil {
		return 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests, b.tokens
}
