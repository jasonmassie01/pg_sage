package store

import (
	"strings"
	"testing"
)

// vacuum-08 review: an API override of analyzer.wraparound_freeze_share
// is held to the same (0, 1] range the config file is; a 0 share would be
// stored, then silently ignored by the rule. No concurrency or database
// test: validation and coercion are pure functions of the value.
func TestWraparoundFreezeShareOverrideRange(t *testing.T) {
	const key = "analyzer.wraparound_freeze_share"
	for _, value := range []string{"0.01", "0.75", "1"} {
		if err := ValidateConfigOverride(key, value); err != nil {
			t.Fatalf("share %s refused: %v", value, err)
		}
	}
	for _, value := range []string{"0", "-0.5", "1.01", "", "abc"} {
		err := ValidateConfigOverride(key, value)
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("share %q: error %v, want a refusal naming the key", value, err)
		}
	}
	if got, ok := coerceValue(key, "0.8").(float64); !ok || got != 0.8 {
		t.Fatalf("coerced share = %#v, want 0.8", coerceValue(key, "0.8"))
	}
}
