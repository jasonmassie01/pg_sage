package config

import "testing"

// migration-05: migration.queue_waiters_threshold defaults to 3 with no
// config file, and an unset, zero or negative value means the default.
func TestMigrationQueueWaitersDefault(t *testing.T) {
	if got := DefaultConfig().Migration.QueueWaiters(); got != 3 {
		t.Fatalf("default QueueWaiters() = %d, want 3", got)
	}
	for _, tc := range []struct{ set, want int }{{0, 3}, {-4, 3}, {1, 1}, {7, 7}} {
		m := MigrationConfig{QueueWaitersThreshold: tc.set}
		if got := m.QueueWaiters(); got != tc.want {
			t.Fatalf("QueueWaitersThreshold=%d: QueueWaiters() = %d, want %d",
				tc.set, got, tc.want)
		}
	}
}
