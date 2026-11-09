package store

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// analyzer.inactive_slot_min_minutes is an API-settable number of minutes,
// 1 up to one week (the YAML range); it is listed with its value. A larger
// override would silently stop inactive_slot from ever firing.
func TestInactiveSlotMinMinutesOverride(t *testing.T) {
	const key = "analyzer.inactive_slot_min_minutes"
	for _, ok := range []string{"1", "5", "1440", "10080"} {
		if err := ValidateConfigOverride(key, ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"0", "-1", "five", "", "1.5", "10081", "1000000"} {
		if err := ValidateConfigOverride(key, bad); err == nil ||
			!strings.Contains(err.Error(), key) {
			t.Errorf("%q: err = %v, want a refusal naming the key", bad, err)
		}
	}
	cfg := config.DefaultConfig()
	cfg.Analyzer.InactiveSlotMinMinutes = 17
	m := map[string]any{}
	addAnalyzerFields(m, &cfg.Analyzer)
	field, ok := m[key].(map[string]any)
	if !ok || field["value"] != 17 {
		t.Fatalf("listed field = %#v, want value 17", m[key])
	}
}
