package tuning

import (
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
)

// Per-database, per-cycle LLM budget (owner decision 5): a request cap and
// a token cap, charged by the LLM client before provider I/O.

var _ llm.Budgeter = (*CycleBudget)(nil)

func TestCycleBudget_RequestCapBoundary(t *testing.T) {
	b := NewCycleBudget(3, 1000)
	for i := 0; i < 3; i++ {
		if !b.TakeRequest() {
			t.Fatalf("request %d of 3 refused", i+1)
		}
	}
	if b.TakeRequest() {
		t.Fatal("the fourth request must be refused")
	}
	if req, _ := b.Used(); req != 3 {
		t.Fatalf("requests used = %d, want 3 (a refusal is not counted)", req)
	}
}

func TestCycleBudget_TokenCapBoundary(t *testing.T) {
	b := NewCycleBudget(10, 1000)
	if !b.CanSpend(1000) {
		t.Fatal("exactly the cap fits")
	}
	if b.CanSpend(1001) {
		t.Fatal("one token over the cap does not fit")
	}
	b.Spend(600)
	if b.CanSpend(401) || !b.CanSpend(400) {
		t.Fatal("remaining 400 tokens")
	}
	b.Spend(-200) // a reservation released after a short reply
	if _, tok := b.Used(); tok != 400 {
		t.Fatalf("tokens used = %d, want 400", tok)
	}
	if !b.CanSpend(600) {
		t.Fatal("released tokens are spendable again")
	}
}

func TestCycleBudget_ZeroAndNegativeLimitsAllowNothing(t *testing.T) {
	for _, b := range []*CycleBudget{NewCycleBudget(0, 0), NewCycleBudget(-1, -5)} {
		if b.TakeRequest() || b.CanSpend(1) {
			t.Fatalf("a zero budget must refuse: %+v", b)
		}
	}
	var nilBudget *CycleBudget
	if nilBudget.TakeRequest() || nilBudget.CanSpend(1) {
		t.Fatal("a nil budget refuses")
	}
	nilBudget.Spend(10) // must not panic
	if req, tok := nilBudget.Used(); req != 0 || tok != 0 {
		t.Fatal("a nil budget used nothing")
	}
}

func TestCycleBudget_NegativeSpendNeverGoesBelowZero(t *testing.T) {
	b := NewCycleBudget(1, 100)
	b.Spend(-50)
	if _, tok := b.Used(); tok != 0 {
		t.Fatalf("tokens = %d, want 0", tok)
	}
	if b.CanSpend(101) {
		t.Fatal("a release must not raise the cap")
	}
}

func TestCycleBudget_ConcurrentUse(t *testing.T) {
	b := NewCycleBudget(100, 100000)
	var wg sync.WaitGroup
	var mu sync.Mutex
	taken := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.TakeRequest() {
				mu.Lock()
				taken++
				mu.Unlock()
			}
			if b.CanSpend(10) {
				b.Spend(10)
			}
		}()
	}
	wg.Wait()
	req, tok := b.Used()
	if taken != 100 || req != 100 {
		t.Fatalf("taken %d used %d, want exactly the cap 100", taken, req)
	}
	if tok > 100000 || tok%10 != 0 {
		t.Fatalf("tokens = %d", tok)
	}
}
