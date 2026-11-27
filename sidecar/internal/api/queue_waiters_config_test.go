package api

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// migration.queue_waiters_threshold can be written through the API: the
// detector reads the live configuration on every poll.
func TestHotReloadMigration_QueueWaitersThreshold(t *testing.T) {
	cfg := config.DefaultConfig()
	for _, tc := range []struct {
		value string
		want  int
	}{{"5", 5}, {"1", 1}, {"3", 3}} {
		hotReloadMigration(cfg, "migration.queue_waiters_threshold", tc.value)
		if cfg.Migration.QueueWaitersThreshold != tc.want {
			t.Fatalf("after %q: %d, want %d", tc.value,
				cfg.Migration.QueueWaitersThreshold, tc.want)
		}
	}
}
