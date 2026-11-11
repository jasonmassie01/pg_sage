package tuning

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// indexes-12: the agent reports a cycle in which its model could not be
// consulted (daily budget spent or unreadable, every conversation failing:
// breaker open, provider down), so the deterministic index floor runs in
// its place. A cycle in which the model answered, or had nothing to be
// asked, is not one.

func tuneOut(t *testing.T, a *Agent) bool {
	t.Helper()
	prev, cur := threeStatementPair()
	out, err := a.Tune(context.Background(), cur, prev)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	return out.ModelUnavailable
}

func TestTune_ModelUnavailableWhenTheDayIsSpent(t *testing.T) {
	s := defaultSettings()
	s.DailyTokenLimit = 250
	s.MaxOutputTokens = 100
	h := newHarnessWith(t, s)
	h.store.day = map[string][2]int64{t0.UTC().Format(time.DateOnly): {250, 3}}
	if !tuneOut(t, h.agent) {
		t.Fatal("a spent day is a cycle without the model")
	}
	if h.model.callCount() != 0 {
		t.Fatalf("the model was asked %d times on a spent day", h.model.callCount())
	}
}

func TestTune_ModelUnavailableWhenTheDayIsUnreadable(t *testing.T) {
	s := defaultSettings()
	s.DailyTokenLimit = 250
	h := newHarnessWith(t, s)
	h.store.dayErr = errFake
	if !tuneOut(t, h.agent) {
		t.Fatal("an unreadable day asks no model")
	}
}

func TestTune_ModelUnavailableWhenEveryConversationFails(t *testing.T) {
	open := errors.New("llm: circuit breaker open")
	h := newHarness(t, failing(open), failing(open), failing(open), failing(open),
		failing(open), failing(open))
	if !tuneOut(t, h.agent) {
		t.Fatal("a breaker that refuses every case is a cycle without the model")
	}
	if h.model.callCount() == 0 {
		t.Fatal("the cases were asked")
	}
}

func TestTune_ModelAvailableWhenOneCaseIsAnswered(t *testing.T) {
	h := newHarness(t, failing(errors.New("provider 503")))
	if tuneOut(t, h.agent) {
		t.Fatal("the model answered the later cases; the floor must not run")
	}
	h = newHarness(t)
	if tuneOut(t, h.agent) {
		t.Fatal("a model that answered every case is available")
	}
}

func TestTune_NoCasesIsNotModelUnavailable(t *testing.T) {
	h := newHarness(t)
	out, err := h.agent.Tune(context.Background(), &collector.Snapshot{CollectedAt: t0}, nil)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	if out.ModelUnavailable || h.model.callCount() != 0 {
		t.Fatalf("nothing to ask is not an unavailable model: %+v", out)
	}
}
