package store

import (
	"context"
	"time"
)

// StartActionExpiry runs a goroutine that expires stale queue
// items every hour.
func StartActionExpiry(
	ctx context.Context,
	s *ActionStore,
	logFn func(string, string, ...any),
) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			expired, err := s.ExpireStale(ctx)
			if err != nil {
				logFn("action_expiry",
					"failed to expire stale actions: %v", err)
				continue
			}
			if expired > 0 {
				logFn("action_expiry",
					"expired %d stale action(s)", expired)
			}
		case <-ctx.Done():
			return
		}
	}
}
