package llm

import (
	"context"
	"testing"
	"time"
)

// Probe is admitted as the half-open probe inside the cooldown and
// closes the breaker when the provider answers.
func TestProbe_AdmittedInsideCooldown(t *testing.T) {
	s := newScriptServer(t, step{})
	c := scriptClient(s, "m", 5)
	openFor(c, 0)
	res := c.Probe(context.Background())
	if res.Class != "ok" || res.CircuitState != "closed" || res.Tokens != 10 {
		t.Fatalf("probe = %+v, want ok/closed/10 tokens", res)
	}
	if s.calls.Load() != 1 || c.TokensUsedToday() != 10 {
		t.Fatalf("calls=%d charged=%d, want one charged request",
			s.calls.Load(), c.TokensUsedToday())
	}
}

// Repeated probes are not suppressed by the duplicate-prompt cooldown.
func TestProbe_RepeatsAreSent(t *testing.T) {
	s := newScriptServer(t, step{})
	c := scriptClient(s, "m", 5)
	for i := 0; i < 2; i++ {
		if res := c.Probe(context.Background()); res.Class != "ok" {
			t.Fatalf("probe %d = %+v", i, res)
		}
	}
	if s.calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", s.calls.Load())
	}
}

// A failing probe reopens the breaker and reports the class; the error
// text never carries the API key.
func TestProbe_FailureReopens(t *testing.T) {
	s := newScriptServer(t, step{status: 503})
	c := scriptClient(s, "m", 5)
	openFor(c, time.Hour)
	res := c.Probe(context.Background())
	if res.Class != "server" || res.CircuitState != "open" || res.Error == "" {
		t.Fatalf("probe = %+v, want server/open with an error", res)
	}
	if !c.IsCircuitOpen() {
		t.Fatal("failed probe left the breaker admitting requests")
	}
}

// A probe on a disabled client sends nothing.
func TestProbe_Disabled(t *testing.T) {
	c := New(nil, noopLog)
	if res := c.Probe(context.Background()); res.Class != "disabled" {
		t.Fatalf("probe = %+v, want disabled", res)
	}
	var nilClient *Client
	if res := nilClient.Probe(context.Background()); res.Class != "disabled" {
		t.Fatalf("nil probe = %+v, want disabled", res)
	}
}

// Nil/empty: a nil context (allowed by ChatResult) is not a forced probe.
func TestForcedProbe_NilContext(t *testing.T) {
	//nolint:staticcheck // nil is what a careless caller passes.
	if forcedProbe(nil) {
		t.Fatal("nil context reported a forced probe")
	}
	if !forcedProbe(withForcedProbe(context.Background())) {
		t.Fatal("forced probe context not recognised")
	}
	if forcedProbe(context.Background()) {
		t.Fatal("plain context reported a forced probe")
	}
}
