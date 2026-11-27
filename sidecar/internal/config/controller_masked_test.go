package config

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// config-03: the controller logs each masked override once per distinct
// (file value, override value) pair, naming the key, the user and the time.

type maskedLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *maskedLog) add(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg)
}

func (l *maskedLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func maskedCandidate(file, override string, by int) *Config {
	cfg := DefaultConfig()
	cfg.Collector.BatchSize = 300
	at := time.Date(2026, 11, 10, 8, 30, 0, 0, time.UTC)
	cfg.SetOverrideMask(DefaultConfig(), []MaskedKey{{Key: "collector.batch_size",
		FileValue: file, OverrideValue: override, EffectiveValue: override,
		UpdatedBy: by, UpdatedAt: at}})
	return cfg
}

func TestMaskedOverrideLoggedOncePerValuePair(t *testing.T) {
	controller := NewConfigController(maskedCandidate("100", "200", 7), nil)
	log := &maskedLog{}
	controller.SetMaskedOverrideLogger(log.add)
	lines := log.all()
	if len(lines) != 1 {
		t.Fatalf("startup notices = %q, want exactly one", lines)
	}
	for _, want := range []string{"collector.batch_size", "100", "200", "user 7",
		"2026-11-10T08:30:00Z"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("notice %q does not contain %q", lines[0], want)
		}
	}
	apply := func(candidate *Config) {
		t.Helper()
		desired := controller.Desired().Generation
		if _, err := controller.Apply(context.Background(), desired, candidate); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	// Same pair again (the candidate differs elsewhere so it publishes).
	same := maskedCandidate("100", "200", 7)
	same.Analyzer.IntervalSeconds = 900
	apply(same)
	if got := log.all(); len(got) != 1 {
		t.Fatalf("the same pair was logged again: %q", got)
	}
	changed := maskedCandidate("150", "200", 7)
	apply(changed)
	if got := log.all(); len(got) != 2 || !strings.Contains(got[1], "150") {
		t.Fatalf("a new file value was not logged once: %q", got)
	}
}

func TestMaskedOverrideTrustNoticeSaysRestrictiveWins(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SetOverrideMask(DefaultConfig(), []MaskedKey{{Key: "trust.level",
		FileValue: "observation", OverrideValue: "autonomous",
		EffectiveValue: "observation", UpdatedBy: 3}})
	controller := NewConfigController(cfg, nil)
	log := &maskedLog{}
	controller.SetMaskedOverrideLogger(log.add)
	lines := log.all()
	if len(lines) != 1 || !strings.Contains(lines[0], "restrictive") ||
		!strings.Contains(lines[0], "effective \"observation\"") {
		t.Fatalf("trust notice = %q, want one line naming the restrictive rule", lines)
	}
}

func TestMaskedOverrideNothingMaskedLogsNothing(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	log := &maskedLog{}
	controller.SetMaskedOverrideLogger(log.add)
	controller.SetMaskedOverrideLogger(nil) // nil restores the default sink
	if got := log.all(); len(got) != 0 {
		t.Fatalf("notices without a masked key: %q", got)
	}
	if got := controller.MaskedByOverride(); got != nil {
		t.Fatalf("MaskedByOverride = %v, want nil", got)
	}
}

func TestMaskedOverrideConcurrentNoticesLogOnce(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	log := &maskedLog{}
	controller.SetMaskedOverrideLogger(log.add)
	cfg := maskedCandidate("100", "200", 1)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			controller.noteMasked(cfg)
		}()
	}
	wg.Wait()
	if got := log.all(); len(got) != 1 {
		t.Fatalf("16 concurrent notices logged %d lines, want 1: %q", len(got), got)
	}
}

func TestControllerMaskedByOverrideFollowsDesired(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	candidate := maskedCandidate("100", "200", 2)
	if _, err := controller.Apply(context.Background(), 1, candidate); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := controller.MaskedByOverride()
	if len(got) != 1 || got[0].Key != "collector.batch_size" || got[0].FileValue != "100" {
		t.Fatalf("MaskedByOverride = %+v, want the desired revision's list", got)
	}
}
