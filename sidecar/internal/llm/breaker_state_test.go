package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// openFor opens c's breaker as if it opened ago.
func openFor(c *Client, ago time.Duration) {
	for i := 0; i < failureThreshold; i++ {
		c.recordFailure()
	}
	c.mu.Lock()
	c.openedAt = time.Now().Add(-ago)
	c.mu.Unlock()
}

func TestBreaker_ReconfigureNewKeyCloses(t *testing.T) {
	s := newScriptServer(t, step{status: 401})
	c := scriptClient(s, "m", 5)
	if _, _, err := c.Chat(context.Background(), "s", "u", 10); err == nil {
		t.Fatal("401 accepted")
	}
	if circuitState(c) != "open" {
		t.Fatalf("state = %s, want open", circuitState(c))
	}
	cfg, _ := c.configSnapshot()
	cfg.APIKey = "fixed-key"
	c.Reconfigure(&cfg)
	if circuitState(c) != "closed" || c.IsCircuitOpen() || failureCount(c) != 0 {
		t.Fatalf("state=%s open=%v failures=%d, want closed after a key change",
			circuitState(c), c.IsCircuitOpen(), failureCount(c))
	}
}

// Changing only the budget or cooldown keeps an open breaker open; a
// model, endpoint or auth change closes it.
func TestBreaker_ReconfigureResetsOnlyOnProviderChange(t *testing.T) {
	mutate := map[string]func(*config.LLMConfig){
		"budget":   func(c *config.LLMConfig) { c.TokenBudgetDaily = 5 },
		"cooldown": func(c *config.LLMConfig) { c.CooldownSeconds = 600 },
		"model":    func(c *config.LLMConfig) { c.Model = "other" },
		"endpoint": func(c *config.LLMConfig) { c.Endpoint += "/v2" },
		"auth":     func(c *config.LLMConfig) { c.Auth = "databricks" },
	}
	wantOpen := map[string]bool{"budget": true, "cooldown": true}
	for name, fn := range mutate {
		t.Run(name, func(t *testing.T) {
			c := scriptClient(newScriptServer(t), "m", 5)
			openFor(c, 0)
			cfg, _ := c.configSnapshot()
			fn(&cfg)
			c.Reconfigure(&cfg)
			if got := c.IsCircuitOpen(); got != wantOpen[name] {
				t.Fatalf("open = %v after %s change, want %v", got, name,
					wantOpen[name])
			}
		})
	}
}

// IsCircuitOpen is a pure read: after the cooldown it reports the
// breaker admits a request, but never closes it or clears failures.
func TestBreaker_IsCircuitOpenDoesNotMutate(t *testing.T) {
	c := scriptClient(newScriptServer(t), "m", 5)
	openFor(c, 2*time.Minute)
	for i := 0; i < 100; i++ {
		if c.IsCircuitOpen() {
			t.Fatalf("read %d: refusing after cooldown", i)
		}
	}
	if circuitState(c) != "open" || failureCount(c) != failureThreshold {
		t.Fatalf("state=%s failures=%d, want still open with %d failures",
			circuitState(c), failureCount(c), failureThreshold)
	}
	// A failing probe reopens at once, without three new failures.
	probe, err := c.admit(false)
	if !probe || err != nil {
		t.Fatalf("admit = %v, %v; want the probe", probe, err)
	}
	c.recordProviderFailure(classServer)
	if !c.IsCircuitOpen() {
		t.Fatal("failed probe did not reopen the breaker")
	}
}

func TestBreaker_ExactlyOneProbeAfterCooldown(t *testing.T) {
	c := scriptClient(newScriptServer(t), "m", 5)
	openFor(c, 2*time.Minute)
	const n = 20
	var wg sync.WaitGroup
	results := make(chan error, n)
	probes := make(chan bool, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			probe, err := c.admit(false)
			probes <- probe
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	close(probes)
	admitted, refused := 0, 0
	for p := range probes {
		if p {
			admitted++
		}
	}
	for err := range results {
		if errors.Is(err, ErrCircuitOpen) {
			refused++
		}
	}
	if admitted != 1 || refused != n-1 {
		t.Fatalf("admitted=%d refused=%d, want 1/%d", admitted, refused, n-1)
	}
	if circuitState(c) != "half_open" || !c.IsCircuitOpen() {
		t.Fatalf("state=%s, want half_open refusing others", circuitState(c))
	}
}

// Within the cooldown nothing is admitted.
func TestBreaker_NoProbeWithinCooldown(t *testing.T) {
	c := scriptClient(newScriptServer(t), "m", 5)
	openFor(c, 0)
	if probe, err := c.admit(false); probe || !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("admit = %v, %v; want refused", probe, err)
	}
}

// End to end: the probe's success closes the breaker.
func TestBreaker_ProbeSuccessCloses(t *testing.T) {
	s := newScriptServer(t, step{})
	c := scriptClient(s, "m", 5)
	openFor(c, 2*time.Minute)
	if _, _, err := c.Chat(context.Background(), "s", "u", 10); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if circuitState(c) != "closed" || failureCount(c) != 0 {
		t.Fatalf("state=%s failures=%d, want closed", circuitState(c),
			failureCount(c))
	}
	if StatusOf(c).LastSuccessAt == nil {
		t.Fatal("last_success_at not set by the probe")
	}
}

// A probe that ends without a verdict (a caller rejection) hands the
// probe back: the next request may probe again.
func TestBreaker_ProbeWithoutVerdictIsReleased(t *testing.T) {
	s := newScriptServer(t, step{status: 400}, step{})
	c := scriptClient(s, "m", 5)
	openFor(c, 2*time.Minute)
	_, _, err := c.Chat(context.Background(), "s", "u", 10)
	if !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("err = %v, want the 400", err)
	}
	if circuitState(c) != "open" || c.IsCircuitOpen() {
		t.Fatalf("state=%s open=%v, want open but admitting a new probe",
			circuitState(c), c.IsCircuitOpen())
	}
	if _, _, err := c.Chat(context.Background(), "s", "u2", 10); err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if circuitState(c) != "closed" {
		t.Fatalf("state = %s, want closed", circuitState(c))
	}
}

// Concurrency: 20 requests against a failing provider open the breaker
// exactly once.
func TestBreaker_ConcurrentFailuresOpenOnce(t *testing.T) {
	s := newScriptServer(t, step{status: 500})
	logs := &capLog{}
	c := scriptClient(s, "m", 5)
	c.logFn = logs.log
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = c.Chat(context.Background(), "s", string(rune('a'+i)), 10)
		}()
	}
	wg.Wait()
	if n := logs.count("circuit breaker opened"); n != 1 {
		t.Fatalf("open transitions = %d, want 1 (lines %v)", n, logs.lines)
	}
	if !c.IsCircuitOpen() {
		t.Fatal("breaker not open after 20 failing requests")
	}
}

func TestClientStatus_BreakerFields(t *testing.T) {
	s := newScriptServer(t, step{status: 502})
	c := scriptClient(s, "m", 5)
	fresh := StatusOf(c)
	if fresh.CircuitState != "closed" || fresh.LastErrorClass != "" ||
		fresh.LastErrorAt != nil || fresh.LastSuccessAt != nil ||
		fresh.ConsecutiveFailures != 0 {
		t.Fatalf("fresh status = %+v, want closed and empty", fresh)
	}
	before := time.Now()
	_, _, _ = c.Chat(context.Background(), "s", "u", 10)
	got := StatusOf(c)
	if got.LastErrorClass != "server" || got.ConsecutiveFailures != 1 ||
		got.LastErrorAt == nil || got.LastErrorAt.Before(before) {
		t.Fatalf("status = %+v, want one server failure stamped now", got)
	}
}
