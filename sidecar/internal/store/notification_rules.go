package store

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
)

// CreateRule inserts a new notification rule.
func (s *NotificationStore) CreateRule(
	ctx context.Context,
	channelID int, event, minSeverity string,
) (int, error) {
	if channelID <= 0 {
		return 0, fmt.Errorf("%w: channel_id is required", ErrValidation)
	}
	if err := validateEventType(event); err != nil {
		return 0, err
	}
	if err := validateSeverity(minSeverity); err != nil {
		return 0, err
	}
	if _, err := s.GetChannel(ctx, channelID); err != nil {
		return 0, err
	}

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var id int
	err := s.pool.QueryRow(qctx,
		`INSERT INTO sage.notification_rules
		    (channel_id, event, min_severity)
		 VALUES ($1, $2, $3) RETURNING id`,
		channelID, event, minSeverity,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("inserting rule: %w", err)
	}
	return id, nil
}

// ListRules returns all notification rules.
func (s *NotificationStore) ListRules(
	ctx context.Context,
) ([]notify.Rule, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := s.pool.Query(qctx,
		`SELECT id, channel_id, event, min_severity, enabled
		 FROM sage.notification_rules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing rules: %w", err)
	}
	defer rows.Close()

	var result []notify.Rule
	for rows.Next() {
		var r notify.Rule
		if err := rows.Scan(
			&r.ID, &r.ChannelID, &r.Event,
			&r.MinSeverity, &r.Enabled,
		); err != nil {
			return nil, fmt.Errorf("scanning rule: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// DeleteRule removes a notification rule.
func (s *NotificationStore) DeleteRule(
	ctx context.Context, id int,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		"DELETE FROM sage.notification_rules WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("deleting rule %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: rule %d", ErrNotFound, id)
	}
	return nil
}

// UpdateRule enables or disables a notification rule.
func (s *NotificationStore) UpdateRule(
	ctx context.Context, id int, enabled bool,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`UPDATE sage.notification_rules
		 SET enabled = $1 WHERE id = $2`,
		enabled, id)
	if err != nil {
		return fmt.Errorf("updating rule %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: rule %d", ErrNotFound, id)
	}
	return nil
}
