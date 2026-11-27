package llm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-03 part 1: FleetBudget.ResetDaily wiped the external hold of an
// in-flight call, and the settle charged only actual-hold, so day N+1
// under-counted (clamped to zero when actual < hold).

// heldFleetBudget is the sidecar's dbBudget shape: one database's share
// of a fleet budget, with reset-aware holds.
type heldFleetBudget struct {
	budget   *fleet.FleetBudget
	database string
}

func (b heldFleetBudget) CanSpend(tokens int) bool {
	return b.budget.CanSpend(b.database, tokens)
}
func (b heldFleetBudget) Spend(tokens int) { b.budget.Spend(b.database, tokens) }
func (b heldFleetBudget) Hold(tokens int) (uint64, bool) {
	return b.budget.Hold(b.database, tokens)
}
func (b heldFleetBudget) Settle(held, actual int, epoch uint64) {
	b.budget.Settle(b.database, held, actual, epoch)
}

var _ llm.HoldBudgeter = heldFleetBudget{}

// gatedProvider answers each request after release closes, with status
// code and usage tokens.
func gatedProvider(t *testing.T, status, usage int) (string, chan struct{}, chan struct{}) {
	t.Helper()
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			started <- struct{}{}
			<-release
			if status != http.StatusOK {
				http.Error(w, `{"error":"bad request"}`, status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"ok"},`+
				`"finish_reason":"stop"}],"usage":{"total_tokens":%d}}`, usage)
		}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	return server.URL, started, release
}

func fleetHoldClient(url string, shared *fleet.FleetBudget) *llm.Client {
	c := llm.New(&config.LLMConfig{
		Enabled: true, Endpoint: url, APIKey: "test-key", Model: "test-model",
		TimeoutSeconds: 5, TokenBudgetDaily: 1000000,
	}, func(string, string, ...any) {})
	c.SetBudget(heldFleetBudget{budget: shared, database: "orders"})
	return c
}

func runHeldFleetCall(
	t *testing.T, c *llm.Client, started chan struct{},
) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Chat(context.Background(), "sys", "a fleet prompt", 4096)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("call never reached the provider")
	}
	return done
}

func TestBudgetHold_FleetResetMidFlightChargesFullActual(t *testing.T) {
	url, started, release := gatedProvider(t, http.StatusOK, 300)
	shared := fleet.NewBudget(100000, []string{"orders"})
	c := fleetHoldClient(url, shared)
	done := runHeldFleetCall(t, c, started)
	if held := shared.Used("orders"); held <= 300 {
		t.Fatalf("precondition: hold = %d, want the estimate above 300", held)
	}

	shared.ResetDaily()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got := shared.Used("orders"); got != 300 {
		t.Fatalf("fleet used after reset mid-flight = %d, want actual 300", got)
	}
}

func TestBudgetHold_FleetSettleWithoutResetChargesActual(t *testing.T) {
	url, started, release := gatedProvider(t, http.StatusOK, 300)
	shared := fleet.NewBudget(100000, []string{"orders"})
	c := fleetHoldClient(url, shared)
	done := runHeldFleetCall(t, c, started)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got := shared.Used("orders"); got != 300 {
		t.Fatalf("fleet used = %d, want actual 300", got)
	}
}

// A failed call after a reset must not subtract its hold from spend
// recorded since the reset.
func TestBudgetHold_FleetReleaseAfterResetKeepsNewSpend(t *testing.T) {
	url, started, release := gatedProvider(t, http.StatusBadRequest, 0)
	shared := fleet.NewBudget(100000, []string{"orders"})
	c := fleetHoldClient(url, shared)
	done := runHeldFleetCall(t, c, started)

	shared.ResetDaily()
	shared.Spend("orders", 50)
	close(release)
	if err := <-done; err == nil {
		t.Fatal("a 400 reply must fail the call")
	}
	if got := shared.Used("orders"); got != 50 {
		t.Fatalf("fleet used = %d, want the 50 spent after the reset", got)
	}
}
