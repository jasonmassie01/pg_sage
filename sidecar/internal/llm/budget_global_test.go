package llm_test

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-03 part 2: one process-wide Budget caps the sum of every client's
// spend. Ceiling 0 is unlimited.

func globalClients(url string, n int, global *llm.Budget) []*llm.Client {
	clients := make([]*llm.Client, n)
	for i := range clients {
		clients[i] = ceilingClient(url, 1000000)
		clients[i].SetGlobalBudget(global)
	}
	return clients
}

func TestGlobalBudget_ThreeClientsSpendExactlyTheCeiling(t *testing.T) {
	url, calls := countingProvider(t, 100)
	global := llm.NewBudget()
	global.SetCeiling(1000, "configured")
	clients := globalClients(url, 3, global)

	for i := 0; i < 10; i++ {
		if err := chatNeeding(clients[i%3], 100, fmt.Sprintf("call %d", i)); err != nil {
			t.Fatalf("call %d of 10 under a 1,000 ceiling: %v", i, err)
		}
	}
	status := global.Status()
	if status.Used != 1000 || status.Ceiling != 1000 || status.Source != "configured" {
		t.Fatalf("global status = %+v, want used 1000 of ceiling 1000", status)
	}
	err := chatNeeding(clients[1], 1, "the 1,001st token")
	if !errors.Is(err, llm.ErrBudgetExhausted) {
		t.Fatalf("1,001st token = %v, want ErrBudgetExhausted", err)
	}
	if calls.Load() != 10 {
		t.Fatalf("provider calls = %d, want 10 (refusal before I/O)", calls.Load())
	}
	if !status.Exhausted {
		t.Fatal("a spent ceiling must report exhausted")
	}
}

func TestGlobalBudget_ConcurrentClientsNeverExceedCeiling(t *testing.T) {
	url, _ := countingProvider(t, 100)
	global := llm.NewBudget()
	global.SetCeiling(1000, "configured")
	clients := globalClients(url, 5, global)

	var ok, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := chatNeeding(clients[i%5], 100, fmt.Sprintf("racer %d", i))
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, llm.ErrBudgetExhausted):
				refused.Add(1)
			default:
				t.Errorf("racer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 10 || refused.Load() != 40 {
		t.Fatalf("admitted %d, refused %d; want exactly 10 and 40",
			ok.Load(), refused.Load())
	}
	status := global.Status()
	if status.Used != 1000 || status.Reserved != 0 {
		t.Fatalf("global = %+v, want used 1000 and nothing held", status)
	}
	var perClient int64
	for _, c := range clients {
		perClient += c.TokensUsedToday()
	}
	if perClient != 1000 {
		t.Fatalf("sum of client spend = %d, want 1000", perClient)
	}
}

func TestGlobalBudget_ZeroCeilingIsUnlimited(t *testing.T) {
	url, _ := countingProvider(t, 100)
	global := llm.NewBudget()
	global.SetCeiling(0, "derived")
	clients := globalClients(url, 2, global)
	for i := 0; i < 20; i++ {
		if err := chatNeeding(clients[i%2], 100, fmt.Sprintf("u%d", i)); err != nil {
			t.Fatalf("call %d with an unlimited ceiling: %v", i, err)
		}
	}
	status := global.Status()
	if status.Used != 2000 || status.Ceiling != 0 || status.Exhausted {
		t.Fatalf("global = %+v, want used 2000, unlimited, not exhausted", status)
	}
}

// A per-client refusal must not leave a global hold behind, and a failed
// call must release its global hold.
func TestGlobalBudget_RefusalsAndFailuresReleaseTheHold(t *testing.T) {
	global := llm.NewBudget()
	global.SetCeiling(1000, "configured")
	url, _ := countingProvider(t, 10)
	small := ceilingClient(url, 50)
	small.SetGlobalBudget(global)
	if err := chatNeeding(small, 51, "over the client cap"); !errors.Is(err,
		llm.ErrBudgetExhausted) {
		t.Fatalf("51 under a client cap of 50 = %v", err)
	}
	badURL, _, release := gatedProvider(t, http.StatusBadRequest, 0)
	close(release)
	failing := ceilingClient(badURL, 1000000)
	failing.SetGlobalBudget(global)
	if err := chatNeeding(failing, 400, "fails"); err == nil {
		t.Fatal("a 400 reply must fail the call")
	}
	if s := global.Status(); s.Used != 0 || s.Reserved != 0 {
		t.Fatalf("global = %+v, want nothing used or held", s)
	}
}

// Reset zeroes spend; a lowered ceiling binds the next request.
func TestGlobalBudget_ResetAndLoweredCeiling(t *testing.T) {
	url, _ := countingProvider(t, 100)
	global := llm.NewBudget()
	global.SetCeiling(200, "configured")
	c := globalClients(url, 1, global)[0]
	for i := 0; i < 2; i++ {
		if err := chatNeeding(c, 100, fmt.Sprintf("r%d", i)); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if err := chatNeeding(c, 1, "over"); !errors.Is(err, llm.ErrBudgetExhausted) {
		t.Fatalf("over the ceiling = %v", err)
	}
	global.Reset()
	if s := global.Status(); s.Used != 0 {
		t.Fatalf("used after reset = %d, want 0", s.Used)
	}
	global.SetCeiling(50, "configured")
	if err := chatNeeding(c, 51, "over the lowered ceiling"); !errors.Is(err,
		llm.ErrBudgetExhausted) {
		t.Fatalf("51 under a lowered 50 = %v", err)
	}
	if err := chatNeeding(c, 50, "fits"); err != nil {
		t.Fatalf("50 under 50: %v", err)
	}
}

// A nil Budget is a no-op, so clients outside the registry keep working.
func TestGlobalBudget_NilIsNoOp(t *testing.T) {
	url, _ := countingProvider(t, 100)
	c := ceilingClient(url, 1000)
	c.SetGlobalBudget(nil)
	if err := chatNeeding(c, 100, "no global"); err != nil {
		t.Fatalf("nil global budget: %v", err)
	}
	var nilBudget *llm.Budget
	if s := nilBudget.Status(); s.Ceiling != 0 || s.Used != 0 {
		t.Fatalf("nil status = %+v, want zero", s)
	}
}
