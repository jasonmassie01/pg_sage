package llm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Two goroutines (or two fleet clients) adapting the same model at the
// same moment both succeed, the memory stays consistent under -race and
// the adaptation is logged once.
func TestChat_ConcurrentAdaptationIsRaceFree(t *testing.T) {
	arrived := make(chan struct{}, 8)
	release := make(chan struct{})
	f := newFakeOpenAI(t, func(f *fakeOpenAI) {
		f.maxTokensErr = openAIMaxTokensErr
		f.gate = func() {
			arrived <- struct{}{}
			select {
			case <-release:
			case <-time.After(3 * time.Second):
			}
		}
	})
	logs := &capLog{}
	clients := []*Client{wireClient(f.srv.URL, "gpt-6-luna", "", "", logs.log),
		wireClient(f.srv.URL, "gpt-6-luna", "", "", logs.log)}
	var wg sync.WaitGroup
	errs := make([]error, len(clients))
	for i, c := range clients {
		wg.Add(1)
		go func(i int, c *Client) {
			defer wg.Done()
			_, _, errs[i] = c.Chat(context.Background(), "sys", fmt.Sprint("u", i), 100)
		}(i, c)
	}
	for range clients {
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
			t.Fatal("both rejections did not arrive together")
		}
	}
	close(release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if f.calls.Load() != 4 {
		t.Fatalf("requests = %d, want 4 (each rejected once, retried once)",
			f.calls.Load())
	}
	if n := logs.count("max_completion_tokens"); n != 1 {
		t.Fatalf("adaptation logged %d times, want 1", n)
	}
	for _, c := range clients {
		if failureCount(c) != 0 {
			t.Fatalf("breaker failures = %d, want 0", failureCount(c))
		}
	}
}

// An adaptation 400 never pushes a breaker that is one failure from
// opening over the edge.
func TestChat_AdaptationDoesNotTripNearlyOpenBreaker(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) { f.maxTokensErr = openAIMaxTokensErr })
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", noopLog)
	c.mu.Lock()
	c.failures = 2
	c.mu.Unlock()
	if _, _, err := c.Chat(context.Background(), "sys", "u", 100); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if c.IsCircuitOpen() || failureCount(c) != 0 {
		t.Fatalf("open=%v failures=%d, want closed and reset by the success",
			c.IsCircuitOpen(), failureCount(c))
	}
}

// The per-database budget is reserved once and reconciled to the
// successful response's usage: the rejected attempt is free.
func TestChat_AdaptationChargesExternalBudgetOnce(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) {
		f.maxTokensErr, f.reasoning = openAIMaxTokensErr, 0
	})
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", noopLog)
	ledger := &ledgerBudget{limit: 1_000_000}
	c.SetBudget(ledger)
	if _, _, err := c.Chat(context.Background(), "sys", "u", 100); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	net, deltas := ledger.snapshot()
	if net != 120 || len(deltas) != 2 {
		t.Fatalf("per-database ledger net=%d deltas=%v, want 120 via one "+
			"reservation and one reconciliation", net, deltas)
	}
}

// A request cooldown still applies to an adapted call: the identical
// prompt is suppressed after it succeeded.
func TestChat_AdaptedCallKeepsCooldownSemantics(t *testing.T) {
	f := newFakeOpenAI(t, func(f *fakeOpenAI) { f.maxTokensErr = openAIMaxTokensErr })
	c := wireClient(f.srv.URL, "gpt-6-luna", "", "", noopLog)
	c.cfg.CooldownSeconds = 60
	if _, _, err := c.Chat(context.Background(), "sys", "same", 100); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	_, _, err := c.Chat(context.Background(), "sys", "same", 100)
	if !errors.Is(err, ErrRequestCooldown) {
		t.Fatalf("err = %v, want ErrRequestCooldown", err)
	}
	if f.calls.Load() != 2 {
		t.Fatalf("requests = %d, want 2", f.calls.Load())
	}
}
