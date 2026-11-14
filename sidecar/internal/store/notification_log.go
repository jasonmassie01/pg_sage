package store

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
)

// UnknownLogDatabase names the database of a delivery logged without one:
// a fleet-wide event, or a row written before alerting-06c.
const UnknownLogDatabase = "unknown"

const listLogColumns = `/* pg_sage */ SELECT id, channel_id, event, subject,
        COALESCE(body, ''), status, COALESCE(error, ''), sent_at,
        COALESCE(database, $2), COALESCE(dedup_key, '')
   FROM sage.notification_log`

// ListLogFor returns the most recent notification log entries for
// database, newest first; "" lists every database, including deliveries
// logged without one. A database filter never matches those.
func (s *NotificationStore) ListLogFor(
	ctx context.Context, database string, limit int,
) ([]NotificationLogEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	query := listLogColumns + ` ORDER BY sent_at DESC LIMIT $1`
	args := []any{limit, UnknownLogDatabase}
	if database != "" {
		query = listLogColumns + ` WHERE database = $3 ORDER BY sent_at DESC LIMIT $1`
		args = append(args, database)
	}
	rows, err := s.pool.Query(qctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing notification log (database %q): %w", database, err)
	}
	defer rows.Close()

	var result []NotificationLogEntry
	for rows.Next() {
		var e NotificationLogEntry
		if err := rows.Scan(
			&e.ID, &e.ChannelID, &e.Event, &e.Subject, &e.Body, &e.Status,
			&e.Error, &e.SentAt, &e.Database, &e.DedupKey,
		); err != nil {
			return nil, fmt.Errorf("scanning log entry: %w", err)
		}
		// Rows written before G7-B10 can embed webhook URLs.
		e.Error = notify.RedactURLs(e.Error)
		result = append(result, e)
	}
	return result, rows.Err()
}
