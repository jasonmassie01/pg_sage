package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// llm-03 part 1: a UTC rollover or a manual ResetBudget zeroed the
// in-flight holds, so the settle that followed drove reservedTokens
// negative and the client over-admitted by the in-flight max_tokens.

const holdTokens = 15000

// blockingProvider answers the first request only after release is
// closed, and every later one at once, each with usage tokens.
func blockingProvider(
	t *testing.T, usage int,
) (url string, started <-chan struct{}, release chan struct{}) {
	t.Helper()
	startedCh := make(chan struct{}, 1)
	releaseCh := make(chan struct{})
	first := make(chan struct{}, 1)
	first <- struct{}{}
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			select {
			case <-first:
				startedCh <- struct{}{}
				<-releaseCh
			default:
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":"ok"},`+
				`"finish_reason":"stop"}],"usage":{"total_tokens":%d}}`, usage)
		}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		select {
		case <-releaseCh:
		default:
			close(releaseCh)
		}
	})
	return server.URL, startedCh, releaseCh
}

func holdTestClient(url string, daily int) (*Client, config.LLMConfig) {
	cfg := config.LLMConfig{
		Enabled: true, Endpoint: url, APIKey: "test-key", Model: "test-model",
		TimeoutSeconds: 5, TokenBudgetDaily: daily,
	}
	return New(&cfg, func(string, string, ...any) {}), cfg
}

// startHeldCall runs one Chat holding holdTokens until release closes.
func startHeldCall(t *testing.T, c *Client, started <-chan struct{}) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Chat(context.Background(), "sys", "held call", holdTokens)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("held call never reached the provider")
	}
	return done
}

func reservedNow(c *Client) int64 {
	c.budgetMu.Lock()
	defer c.budgetMu.Unlock()
	return c.reservedTokens
}

func assertHoldSurvives(t *testing.T, c *Client, cfg config.LLMConfig) {
	t.Helper()
	if got := reservedNow(c); got != holdTokens {
		t.Fatalf("reserved after reset = %d, want the in-flight %d", got, holdTokens)
	}
	// 15,000 held + 6,000 > 20,000: the hold must still count.
	r, err := c.reserveBudget(cfg, 6000, 0)
	if err == nil {
		c.releaseBudget(r)
		t.Fatal("reset forgot the in-flight hold and admitted 6,000 over the cap")
	}
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("refusal = %v, want ErrBudgetExhausted", err)
	}
}

func assertSettled(t *testing.T, c *Client, done <-chan error, release chan struct{}) {
	t.Helper()
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("held call: %v", err)
	}
	if got := reservedNow(c); got != 0 {
		t.Fatalf("reserved after settle = %d, want 0 (never negative)", got)
	}
	if got := c.TokensUsedToday(); got != 100 {
		t.Fatalf("used after settle = %d, want the call's 100", got)
	}
}

func TestBudgetHold_RolloverKeepsInFlightHold(t *testing.T) {
	url, started, release := blockingProvider(t, 100)
	c, cfg := holdTestClient(url, 20000)
	done := startHeldCall(t, c, started)

	// The UTC day turns while the call is in flight.
	c.budgetResetDay.Store(budgetDay(time.Now()) - 1)
	c.tokensUsedToday.Store(4000)

	assertHoldSurvives(t, c, cfg)
	if got := c.TokensUsedToday(); got != 0 {
		t.Fatalf("used after rollover = %d, want 0", got)
	}
	assertSettled(t, c, done, release)
}

func TestBudgetHold_ResetBudgetKeepsInFlightHold(t *testing.T) {
	url, started, release := blockingProvider(t, 100)
	c, cfg := holdTestClient(url, 20000)
	done := startHeldCall(t, c, started)
	c.tokensUsedToday.Store(4000)

	c.ResetBudget()

	if got := c.TokensUsedToday(); got != 0 {
		t.Fatalf("used after ResetBudget = %d, want 0", got)
	}
	assertHoldSurvives(t, c, cfg)
	assertSettled(t, c, done, release)
}

// After a settled call, the full cap is admissible again: a negative
// reserve would have admitted more, a leaked hold less.
func TestBudgetHold_CapExactAfterResetAndSettle(t *testing.T) {
	url, started, release := blockingProvider(t, 100)
	c, cfg := holdTestClient(url, 20000)
	done := startHeldCall(t, c, started)
	c.ResetBudget()
	assertSettled(t, c, done, release)

	r, err := c.reserveBudget(cfg, 19900, 0)
	if err != nil {
		t.Fatalf("19,900 more must fit under 20,000 after 100 used: %v", err)
	}
	c.releaseBudget(r)
	if _, err := c.reserveBudget(cfg, 19901, 0); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("19,901 more = %v, want ErrBudgetExhausted", err)
	}
}

// A failed call releases its hold even after a reset in flight.
func TestBudgetHold_ReleaseAfterResetNeverNegative(t *testing.T) {
	c, cfg := holdTestClient("http://127.0.0.1:1", 20000)
	r, err := c.reserveBudget(cfg, holdTokens, 0)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	c.ResetBudget()
	c.releaseBudget(r)
	if got := reservedNow(c); got != 0 {
		t.Fatalf("reserved after release = %d, want 0", got)
	}
	if got := c.TokensUsedToday(); got != 0 {
		t.Fatalf("used after a released hold = %d, want 0", got)
	}
}
