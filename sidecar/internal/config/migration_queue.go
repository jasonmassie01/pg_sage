package config

// DefaultQueueWaitersThreshold is migration.queue_waiters_threshold's
// default (migration-05).
const DefaultQueueWaitersThreshold = 3

// QueueWaiters is the effective lock-queue threshold: unset, zero or
// negative means DefaultQueueWaitersThreshold.
func (m MigrationConfig) QueueWaiters() int {
	if m.QueueWaitersThreshold <= 0 {
		return DefaultQueueWaitersThreshold
	}
	return m.QueueWaitersThreshold
}
