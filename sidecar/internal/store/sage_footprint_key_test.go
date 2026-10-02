package store

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// retention.sage_size_warning_pct is an API-settable percentage: 0..100,
// 0 disabling the guard; it is listed with its current value.
func TestSageSizeWarningPctOverride(t *testing.T) {
	const key = "retention.sage_size_warning_pct"
	for _, ok := range []string{"0", "10", "100"} {
		if err := ValidateConfigOverride(key, ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"-1", "101", "ten", ""} {
		if err := ValidateConfigOverride(key, bad); err == nil ||
			!strings.Contains(err.Error(), key) {
			t.Errorf("%q: err = %v, want a refusal naming the key", bad, err)
		}
	}
	cfg := config.DefaultConfig()
	cfg.Retention.SageSizeWarningPct = 17
	m := map[string]any{}
	addRetentionFields(m, &cfg.Retention)
	field, ok := m[key].(map[string]any)
	if !ok || field["value"] != 17 {
		t.Fatalf("listed field = %#v, want value 17", m[key])
	}
}
