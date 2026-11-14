package store

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// config-02b: the store validates through config.Registry().

// Error propagation: a stored override that the registry now refuses
// (analyzer.self_cost_budget_ms above the YAML ceiling, which the API
// accepted before) is skipped with a warning naming the key and its
// updated_at, never the value, and a valid save of the key still works.
func TestReplaySkipsAStoredOverrideNowOutOfRange(t *testing.T) {
	const key = "analyzer.self_cost_budget_ms"
	apply, ok, problem := ReplayOverride(key, "5000000")
	if ok || apply != "" || !strings.Contains(problem, "int_nonneg") {
		t.Fatalf("ReplayOverride = %q, %v, %q; want skipped by the int_nonneg check",
			apply, ok, problem)
	}
	at := time.Date(2026, 11, 1, 2, 3, 4, 0, time.UTC)
	msg := StoredOverrideWarning(ConfigOverride{Key: key, Value: "5000000",
		UpdatedAt: at}, problem)
	if !strings.Contains(msg, key) || !strings.Contains(msg, "2026-11-01T02:03:04Z") ||
		strings.Contains(msg, "5000000") {
		t.Fatalf("warning %q must name the key and updated_at, not the value", msg)
	}
	if err := ValidateConfigOverride(key, "4000"); err != nil {
		t.Fatalf("a valid save after the skip failed: %v", err)
	}
}

// A zero:"default" key: a file may hold 0, the API refuses to store it and
// says how to restore the default; replay skips such a row, as before.
func TestZeroDefaultKeysRefuseAStoredZero(t *testing.T) {
	for _, key := range []string{"safety.vacuum_stall_minutes",
		"safety.reindex_timeout_seconds", "analyzer.inactive_slot_min_minutes"} {
		err := ValidateConfigOverride(key, "0")
		if err == nil || !strings.Contains(err.Error(), key) ||
			!strings.Contains(err.Error(), "remove the override") {
			t.Errorf("%s=0: err %v, want a refusal pointing at removal", key, err)
		}
		if _, ok, problem := ReplayOverride(key, "0"); ok || problem == "" {
			t.Errorf("%s=0 replayed (problem %q)", key, problem)
		}
	}
}

func TestValidateConfigValueRefusesLakebaseBranchOnTheAPI(t *testing.T) {
	err := validateConfigValue("clone.provider", "lakebase_branch")
	if err == nil || !strings.Contains(err.Error(), "clone.provider") ||
		!strings.Contains(err.Error(), "YAML") {
		t.Fatalf("lakebase_branch err = %v, want a refusal pointing at YAML", err)
	}
}

// Invalid input: malformed numbers name the key and keep the validate:
// prefix the handlers map to 400.
func TestValidateConfigValueMalformedNumbers(t *testing.T) {
	for key, want := range map[string]string{
		"collector.batch_size":             "collector.batch_size must be an integer",
		"rca.wal_spike_multiplier":         "rca.wal_spike_multiplier must be a number",
		"analyzer.cache_hit_ratio_warning": "analyzer.cache_hit_ratio_warning must be a number",
	} {
		err := validateConfigValue(key, "abc")
		if err == nil || !strings.HasPrefix(err.Error(), "validate: ") ||
			!strings.Contains(err.Error(), want) {
			t.Errorf("%s=abc: err %v, want %q", key, err, want)
		}
	}
	err := validateConfigValue("collector.batch_size", "1073741825")
	if err == nil || err.Error() !=
		"validate: collector.batch_size must be 1-1073741824, got 1073741825" {
		t.Fatalf("range message = %v", err)
	}
	if err := validateConfigValue("collector.batch_size", ""); err == nil {
		t.Fatal("an empty integer validated")
	}
}

func TestCoerceValueFollowsTheRegistry(t *testing.T) {
	if v, ok := coerceValue("collector.batch_size", "12").(int); !ok || v != 12 {
		t.Errorf("int key coerced to %#v", coerceValue("collector.batch_size", "12"))
	}
	if v, ok := coerceValue("rca.wal_spike_multiplier", "2.5").(float64); !ok || v != 2.5 {
		t.Errorf("float key coerced to %#v", coerceValue("rca.wal_spike_multiplier", "2.5"))
	}
	if v, ok := coerceValue("llm.enabled", "TRUE").(bool); !ok || !v {
		t.Errorf("bool key coerced to %#v", coerceValue("llm.enabled", "TRUE"))
	}
	for key, value := range map[string]string{"llm.model": "m", "no.such_key": "7",
		"collector.batch_size": "x"} {
		if got := coerceValue(key, value); got != value {
			t.Errorf("coerceValue(%s, %q) = %#v, want the string back", key, value, got)
		}
	}
}

func TestConfigKeyTypesIsAFreshCopy(t *testing.T) {
	a := AllowedConfigKeysSnapshot()
	a["collector.batch_size"] = "string"
	delete(a, "llm.enabled")
	b := AllowedConfigKeysSnapshot()
	if b["collector.batch_size"] != "int_pos" || b["llm.enabled"] != "bool" || len(b) != 121 {
		t.Fatalf("snapshot shared state: batch_size=%q llm.enabled=%q len=%d",
			b["collector.batch_size"], b["llm.enabled"], len(b))
	}
}

// Concurrent access: validation reads the shared registry from many
// goroutines with the same verdicts.
func TestValidateConfigOverrideConcurrently(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ValidateConfigOverride("collector.interval_seconds", "5") != nil {
				errs <- "5 refused"
			}
			if ValidateConfigOverride("collector.interval_seconds", "4") == nil {
				errs <- "4 accepted"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
