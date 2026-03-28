package auth

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// StartSessionCleaner runs CleanExpiredSessions on the given interval
// until ctx is cancelled.
func StartSessionCleaner(
	ctx context.Context, pool *pgxpool.Pool, interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := CleanExpiredSessions(ctx, pool); err != nil {
				slog.Warn("session cleaner failed",
					"error", err)
			}
		}
	}
}
