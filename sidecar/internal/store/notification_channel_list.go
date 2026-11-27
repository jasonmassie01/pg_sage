package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
//
// The channel is matched by a range, not "=", on purpose. With "=" the
// planner drops channel_id from the ORDER BY as constant and may walk the
// global sent_at index instead, filtering on channel_id: it assumes every
// channel's rows are spread evenly, so a quiet or never-delivered channel
// beside busy ones reads the whole log. Ordering by channel_id first
// leaves only the per-channel index able to deliver the order.
const listChannelsSQL = `/* pg_sage */ SELECT c.id, c.name, c.type, c.config, c.enabled,
       c.managed_by, COALESCE(c.quiet_start, ''), COALESCE(c.quiet_end, ''),
       COALESCE(c.quiet_tz, ''), COALESCE(l.status, ''), COALESCE(l.error, ''), l.sent_at
  FROM sage.notification_channels c
  LEFT JOIN LATERAL (
       SELECT status, error, sent_at
         FROM sage.notification_log
        WHERE channel_id >= c.id AND channel_id <= c.id
          AND sent_at IS NOT NULL
        ORDER BY channel_id, sent_at DESC, id DESC
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
			&ch.ID, &ch.Name, &ch.Type, &cfgJSON, &ch.Enabled, &ch.ManagedBy,
			&ch.QuietStart, &ch.QuietEnd, &ch.QuietTZ,
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

// ErrInvalidQuietHours is a channel quiet window the store refuses.
var ErrInvalidQuietHours = errors.New("invalid quiet hours")

// SetChannelQuietHours stores channel id's own quiet hours (ux-09b): start
// and end HH:MM in the IANA zone tz (UTC when empty). All three empty
// clears them, so the global alerting pair applies again. An invalid
// window is ErrInvalidQuietHours naming the input; an unknown channel is
// ErrNotFound.
func (s *NotificationStore) SetChannelQuietHours(
	ctx context.Context, id int, start, end, tz string,
) error {
	start, end, tz = strings.TrimSpace(start), strings.TrimSpace(end), strings.TrimSpace(tz)
	if _, err := notify.ParseQuietHours(start, end, tz); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQuietHours, err)
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(qctx, `/* pg_sage */ UPDATE sage.notification_channels
		SET quiet_start = NULLIF($2, ''), quiet_end = NULLIF($3, ''), quiet_tz = NULLIF($4, '')
		WHERE id = $1`, id, start, end, tz)
	if err != nil {
		return fmt.Errorf("setting quiet hours of channel %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: channel %d", ErrNotFound, id)
	}
	return nil
}
