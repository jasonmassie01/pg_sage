package llm

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

// Per-call budget (Sage SRE M1): ToolOptions.Budget layers an extra
// Budgeter (an investigation's reservation) on top of the client's daily
// and per-database budgets. It is charged before provider I/O, released
// when the call fails and reconciled to actual usage when it succeeds.

type ledgerBudget struct {
	mu     sync.Mutex
	limit  int
	net    int
	deltas []int
}

func (b *ledgerBudget) CanSpend(tokens int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.net+tokens <= b.limit
}

func (b *ledgerBudget) Spend(tokens int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.net += tokens
	b.deltas = append(b.deltas, tokens)
}

func (b *ledgerBudget) snapshot() (int, []int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.net, append([]int(nil), b.deltas...)
}

func TestChatWithTools_CallBudgetRefusesBeforeProvider(t *testing.T) {
	ts := newToolServer(t, toolReply("done", nil, 50))
	c := toolClient(ts.srv.URL)
	b := &ledgerBudget{limit: 5}
	_, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		ToolOptions{MaxTokens: 256, Budget: b})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if ts.calls.Load() != 0 {
		t.Fatal("a call over its per-call budget reached the provider")
	}
	if net, deltas := b.snapshot(); net != 0 || len(deltas) != 0 {
		t.Fatalf("refused call charged the budget: net=%d deltas=%v", net, deltas)
	}
	if c.TokensUsedToday() != 0 {
		t.Fatal("refused call charged the daily budget")
	}
}

func TestChatWithTools_CallBudgetReconciledToActual(t *testing.T) {
	ts := newToolServer(t, toolReply("done", nil, 77))
	c := toolClient(ts.srv.URL)
	b := &ledgerBudget{limit: 4000}
	if _, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		ToolOptions{MaxTokens: 256, Budget: b}); err != nil {
		t.Fatalf("ChatWithTools: %v", err)
	}
	net, deltas := b.snapshot()
	if net != 77 || len(deltas) != 2 || deltas[0] <= 0 {
		t.Fatalf("net=%d deltas=%v, want a reservation then reconciliation to 77",
			net, deltas)
	}
}

func TestChatWithTools_CallBudgetReleasedOnProviderFailure(t *testing.T) {
	for name, reply := range map[string]func(http.ResponseWriter){
		"429":       statusReply(http.StatusTooManyRequests),
		"500":       statusReply(http.StatusInternalServerError),
		"malformed": func(w http.ResponseWriter) { _, _ = w.Write([]byte("{not json")) },
	} {
		t.Run(name, func(t *testing.T) {
			ts := newToolServer(t, reply)
			c := toolClient(ts.srv.URL)
			b := &ledgerBudget{limit: 4000}
			if _, err := c.ChatWithTools(context.Background(), userMsgs(),
				evidenceTools, ToolOptions{MaxTokens: 256, Budget: b}); err == nil {
				t.Fatal("provider failure returned no error")
			}
			net, deltas := b.snapshot()
			if net != 0 || len(deltas) != 2 || deltas[0] <= 0 {
				t.Fatalf("net=%d deltas=%v, want reserve then release", net, deltas)
			}
		})
	}
}

func TestChatWithTools_CallBudgetReleasedOnTimeout(t *testing.T) {
	ts := newToolServer(t, func(w http.ResponseWriter) {
		time.Sleep(300 * time.Millisecond)
		toolReply("late", nil, 10)(w)
	})
	c := toolClient(ts.srv.URL)
	b := &ledgerBudget{limit: 4000}
	_, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		ToolOptions{MaxTokens: 256, Budget: b, Timeout: 50 * time.Millisecond})
	if err == nil {
		t.Fatal("timeout returned no error")
	}
	if net, _ := b.snapshot(); net != 0 {
		t.Fatalf("timed-out call left %d tokens charged to the call budget", net)
	}
}

// The client's own daily limit is now a typed error too.
func TestChatWithTools_DailyBudgetIsErrBudgetExhausted(t *testing.T) {
	ts := newToolServer(t, toolReply("done", nil, 10))
	c := toolClient(ts.srv.URL)
	c.tokensUsedToday.Store(100000)
	c.budgetResetDay.Store(budgetDay(time.Now()))
	_, err := c.ChatWithTools(context.Background(), userMsgs(), evidenceTools,
		ToolOptions{MaxTokens: 256})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
}
