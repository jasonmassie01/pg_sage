package config

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// config-02 (d): one boolean grammar for the API validator, the setters and
// the read model.
func TestParseBoolAcceptsCaseAndDigitForms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"true", true}, {"True", true}, {"TRUE", true}, {"1", true}, {"t", true},
		{"T", true}, {"tRuE", true},
		{"false", false}, {"False", false}, {"FALSE", false}, {"0", false},
		{"f", false}, {"F", false},
	} {
		got, err := ParseBool(tc.in)
		if err != nil {
			t.Errorf("ParseBool(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBool(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseBoolRejectsEverythingElse(t *testing.T) {
	for _, in := range []string{"", " ", "yes", "no", "on", "off", "2", "-1",
		"TRUE ", " true", "truee", "nil"} {
		if got, err := ParseBool(in); err == nil {
			t.Errorf("ParseBool(%q) = %v, want an error", in, got)
		}
	}
}

// config-02 (c): the controller marks a candidate that fails validation so
// the API can answer 400 instead of 500.
func TestApplyWrapsValidationFailureInErrInvalidCandidate(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	candidate := controller.Desired().Config
	candidate.Analyzer.UnusedIndexWindowDays = 0
	_, err := controller.Apply(context.Background(), 1, candidate)
	if !errors.Is(err, ErrInvalidCandidate) {
		t.Fatalf("Apply error = %v, want ErrInvalidCandidate", err)
	}
	if !strings.Contains(err.Error(), "analyzer.unused_index_window_days") {
		t.Fatalf("Apply error %q does not name the key", err)
	}
	if got := controller.Desired().Generation; got != 1 {
		t.Fatalf("desired generation = %d after a rejected candidate, want 1", got)
	}
}

func TestApplyGenerationConflictIsNotAnInvalidCandidate(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	_, err := controller.Apply(context.Background(), 7, controller.Desired().Config)
	if !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("Apply error = %v, want ErrGenerationConflict", err)
	}
	if errors.Is(err, ErrInvalidCandidate) {
		t.Fatalf("a generation conflict was reported as an invalid candidate: %v", err)
	}
}

func TestApplyNilCandidateIsNotAnInvalidCandidate(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	_, err := controller.Apply(context.Background(), 1, nil)
	if err == nil || errors.Is(err, ErrInvalidCandidate) {
		t.Fatalf("Apply(nil) error = %v, want a non-validation error", err)
	}
}

// State transition: a reload of a file that now has a harmful zero keeps the
// previous generation; the next valid file is published.
func TestWatcherKeepsPreviousConfigWhenReloadHasHarmfulZero(t *testing.T) {
	initial := DefaultConfig()
	bad := Clone(initial)
	bad.Analyzer.UnusedIndexWindowDays = 0
	good := Clone(initial)
	good.Analyzer.UnusedIndexWindowDays = 30
	next := bad
	published := 0
	watcher := NewAcknowledgedWatcherWithLoader("config.yaml", initial,
		func() (*Config, error) { return Clone(next), nil },
		func(*Config) error { published++; return nil },
	)
	watcher.reload()
	if published != 0 {
		t.Fatal("a candidate with unused_index_window_days=0 was published")
	}
	if got := watcher.Current().Analyzer.UnusedIndexWindowDays; got != 7 {
		t.Fatalf("current window = %d after the rejected reload, want 7", got)
	}
	next = good
	watcher.reload()
	if published != 1 {
		t.Fatalf("published = %d after a valid reload, want 1", published)
	}
	if got := watcher.Current().Analyzer.UnusedIndexWindowDays; got != 30 {
		t.Fatalf("current window = %d after the valid reload, want 30", got)
	}
}

// Concurrent access: two writers racing invalid and valid candidates; only
// valid ones advance the generation, and every rejection is classified.
func TestConcurrentInvalidCandidatesNeverAdvanceGeneration(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	unclassified := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidate := controller.Desired().Config
			candidate.Safety.DDLTimeoutSeconds = 0
			_, err := controller.Apply(context.Background(), 1, candidate)
			if !errors.Is(err, ErrInvalidCandidate) {
				mu.Lock()
				unclassified++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if unclassified != 0 {
		t.Fatalf("%d invalid candidates were not ErrInvalidCandidate", unclassified)
	}
	if got := controller.Desired().Generation; got != 1 {
		t.Fatalf("desired generation = %d, want 1", got)
	}
}
