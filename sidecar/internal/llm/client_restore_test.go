package llm_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

// fleet-10: RestoreUsage adds the usage a previous process recorded today
// to a client (and to the process-wide Budget), so a restart does not
// restore the full daily allocation. Another day's usage is ignored.

func utcToday() time.Time { return time.Now().UTC().Truncate(24 * time.Hour) }

func TestClientRestoreUsage_CountsAgainstTheDailyCap(t *testing.T) {
	url, calls := countingProvider(t, 100)
	c := ceilingClient(url, 1000)
	applied, err := c.RestoreUsage(utcToday(), 900)
	if err != nil || !applied {
		t.Fatalf("restore = (%v, %v), want applied", applied, err)
	}
	if got := c.TokensUsedToday(); got != 900 {
		t.Fatalf("tokens used = %d, want 900", got)
	}
	if err := chatNeeding(c, 100, "fits exactly"); err != nil {
		t.Fatalf("900 restored + 100 = the 1000 cap must pass: %v", err)
	}
	err = chatNeeding(c, 1, "one past")
	if !errors.Is(err, llm.ErrBudgetExhausted) {
		t.Fatalf("one token past the cap = %v, want ErrBudgetExhausted", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", calls.Load())
	}
	day, used := c.UsageSnapshot()
	if !day.Equal(utcToday()) || used != 1000 {
		t.Fatalf("snapshot = (%v, %d), want (%v, 1000)", day, used, utcToday())
	}
}

func TestClientRestoreUsage_AnotherDayIsIgnored(t *testing.T) {
	c := ceilingClient("http://127.0.0.1:1", 1000)
	for _, day := range []time.Time{utcToday().Add(-24 * time.Hour),
		utcToday().Add(24 * time.Hour)} {
		applied, err := c.RestoreUsage(day, 500)
		if err != nil || applied {
			t.Fatalf("restore for %v = (%v, %v), want ignored", day, applied, err)
		}
	}
	if got := c.TokensUsedToday(); got != 0 {
		t.Fatalf("tokens used = %d, want 0", got)
	}
}

func TestClientRestoreUsage_NegativeIsRejected(t *testing.T) {
	c := ceilingClient("http://127.0.0.1:1", 1000)
	applied, err := c.RestoreUsage(utcToday(), -1)
	if err == nil || applied {
		t.Fatalf("restore -1 = (%v, %v), want an error", applied, err)
	}
	if got := c.TokensUsedToday(); got != 0 {
		t.Fatalf("tokens used = %d, want 0", got)
	}
}

func TestClientRestoreUsage_AddsToLocalSpend(t *testing.T) {
	url, _ := countingProvider(t, 30)
	c := ceilingClient(url, 1000)
	if err := chatNeeding(c, 10, "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RestoreUsage(utcToday(), 400); err != nil {
		t.Fatal(err)
	}
	if got := c.TokensUsedToday(); got != 430 {
		t.Fatalf("tokens used = %d, want 430 (30 local + 400 restored)", got)
	}
}

// A fresh client has no usage and reports today.
func TestClientUsageSnapshot_FreshClientIsZero(t *testing.T) {
	c := ceilingClient("http://127.0.0.1:1", 1000)
	day, used := c.UsageSnapshot()
	if used != 0 || !day.Equal(utcToday()) {
		t.Fatalf("snapshot = (%v, %d), want (%v, 0)", day, used, utcToday())
	}
}

func TestClientRestoreUsage_ConcurrentWithChat(t *testing.T) {
	url, _ := countingProvider(t, 10)
	c := ceilingClient(url, 1000000)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = chatNeeding(c, 10, "p") }()
		go func() { defer wg.Done(); _, _ = c.RestoreUsage(utcToday(), 5) }()
	}
	wg.Wait()
	if got := c.TokensUsedToday(); got != 20*15 {
		t.Fatalf("tokens used = %d, want %d", got, 20*15)
	}
}

func TestGlobalBudgetRestoreUsage(t *testing.T) {
	url, _ := countingProvider(t, 100)
	global := llm.NewBudget()
	global.SetCeiling(1000, "configured")
	applied, err := global.RestoreUsage(utcToday(), 900)
	if err != nil || !applied {
		t.Fatalf("restore = (%v, %v), want applied", applied, err)
	}
	if s := global.Status(); s.Used != 900 {
		t.Fatalf("global used = %d, want 900", s.Used)
	}
	c := ceilingClient(url, 1000000)
	c.SetGlobalBudget(global)
	if err := chatNeeding(c, 100, "fits"); err != nil {
		t.Fatalf("the last 100 of the ceiling must pass: %v", err)
	}
	if err := chatNeeding(c, 1, "over"); !errors.Is(err, llm.ErrBudgetExhausted) {
		t.Fatalf("past the restored ceiling = %v, want ErrBudgetExhausted", err)
	}
	day, used := global.Usage()
	if !day.Equal(utcToday()) || used != 1000 {
		t.Fatalf("usage = (%v, %d), want (%v, 1000)", day, used, utcToday())
	}
	if applied, _ := global.RestoreUsage(utcToday().Add(-24*time.Hour), 5); applied {
		t.Fatal("yesterday's usage must be ignored")
	}
	if _, err := global.RestoreUsage(utcToday(), -3); err == nil {
		t.Fatal("a negative restore must be rejected")
	}
}

func TestGlobalBudgetRestoreUsage_NilBudget(t *testing.T) {
	var global *llm.Budget
	applied, err := global.RestoreUsage(utcToday(), 10)
	if err != nil || applied {
		t.Fatalf("nil budget restore = (%v, %v), want (false, nil)", applied, err)
	}
	if _, used := global.Usage(); used != 0 {
		t.Fatalf("nil budget usage = %d, want 0", used)
	}
}
