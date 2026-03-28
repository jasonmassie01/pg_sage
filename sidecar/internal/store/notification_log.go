package store

import (
	"context"
	"fmt"
	"time"
)

// ListLog returns the most recent notification log entries.
func (s *NotificationStore) ListLog(
	ctx context.Context, limit int,
) ([]NotificationLogEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := s.pool.Query(qctx,
		`SELECT id, channel_id, event, subject,
		        COALESCE(body, ''), status,
		        COALESCE(error, ''), sent_at
		 FROM sage.notification_log
		 ORDER BY sent_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing notification log: %w", err)
	}
	defer rows.Close()

	var result []NotificationLogEntry
	for rows.Next() {
		var e NotificationLogEntry
		if err := rows.Scan(
			&e.ID, &e.ChannelID, &e.Event, &e.Subject,
			&e.Body, &e.Status, &e.Error, &e.SentAt,
		); err != nil {
			return nil, fmt.Errorf("scanning log entry: %w", err)
		}
		result = append(result, e)
	}
	return result, rows.Err()
}
