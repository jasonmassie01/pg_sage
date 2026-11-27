package store

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// migration.queue_waiters_threshold is an API-settable positive count,
// listed with its value.
func TestQueueWaitersThresholdOverride(t *testing.T) {
	const key = "migration.queue_waiters_threshold"
	for _, ok := range []string{"1", "3", "50"} {
		if err := ValidateConfigOverride(key, ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"0", "-1", "three", "", "2.5"} {
		if err := ValidateConfigOverride(key, bad); err == nil ||
			!strings.Contains(err.Error(), key) {
			t.Errorf("%q: err = %v, want a refusal naming the key", bad, err)
		}
	}
	mg := config.MigrationConfig{QueueWaitersThreshold: 4}
	m := map[string]any{}
	addMigrationFields(m, &mg)
	field, ok := m[key].(map[string]any)
	if !ok || field["value"] != 4 {
		t.Fatalf("listed field = %#v, want value 4", m[key])
	}
}
