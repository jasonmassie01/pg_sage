package store

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
)

// ChannelListing is a channel with its last delivery attempt, the newest
// sage.notification_log row of that channel (alerting-10). A channel that
// was never delivered has an empty LastStatus and a nil LastSentAt.
type ChannelListing struct {
	notify.Channel
	LastStatus string     `json:"last_status"`
	LastError  string     `json:"last_error"`
	LastSentAt *time.Time `json:"last_sent_at"`
}

// listChannelsSQL reads each channel's newest log row through a LATERAL
// probe on idx_notification_log_channel_sent (channel_id, sent_at DESC):
// one index descent per channel, however long the log is.
const listChannelsSQL = `/* pg_sage */ SELECT c.id, c.name, c.type, c.config, c.enabled,
       COALESCE(l.status, ''), COALESCE(l.error, ''), l.sent_at
  FROM sage.notification_channels c
  LEFT JOIN LATERAL (
       SELECT status, error, sent_at
         FROM sage.notification_log
        WHERE channel_id = c.id AND sent_at IS NOT NULL
        ORDER BY sent_at DESC, id DESC
        LIMIT 1) l ON true
 ORDER BY c.id`

// ListChannels returns all notification channels with their last
// delivery attempt. Log errors are redacted again on the way out, since
// rows written before G7-B10 can embed webhook URLs.
func (s *NotificationStore) ListChannels(
	ctx context.Context,
) ([]ChannelListing, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := s.pool.Query(qctx, listChannelsSQL)
	if err != nil {
		return nil, fmt.Errorf("listing channels: %w", err)
	}
	defer rows.Close()

	var result []ChannelListing
	for rows.Next() {
		var ch ChannelListing
		var cfgJSON []byte
		if err := rows.Scan(
			&ch.ID, &ch.Name, &ch.Type, &cfgJSON, &ch.Enabled,
			&ch.LastStatus, &ch.LastError, &ch.LastSentAt,
		); err != nil {
			return nil, fmt.Errorf("scanning channel: %w", err)
		}
		if ch.Config, err = s.decodeConfig(cfgJSON, ch.ID); err != nil {
			return nil, fmt.Errorf("channel %d: %w", ch.ID, err)
		}
		ch.LastError = notify.RedactURLs(ch.LastError)
		result = append(result, ch)
	}
	return result, rows.Err()
}
