package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/notify"
)

// NotificationStore handles CRUD for notification channels, rules,
// and log entries.
type NotificationStore struct {
	pool       *pgxpool.Pool
	dispatcher *notify.Dispatcher
}

// NewNotificationStore creates a NotificationStore.
func NewNotificationStore(
	pool *pgxpool.Pool,
	dispatcher *notify.Dispatcher,
) *NotificationStore {
	return &NotificationStore{
		pool:       pool,
		dispatcher: dispatcher,
	}
}

// NotificationLogEntry represents a delivery log row.
type NotificationLogEntry struct {
	ID        int       `json:"id"`
	ChannelID *int      `json:"channel_id"`
	Event     string    `json:"event"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	Status    string    `json:"status"`
	Error     string    `json:"error"`
	SentAt    time.Time `json:"sent_at"`
}

// CreateChannel inserts a new notification channel.
func (s *NotificationStore) CreateChannel(
	ctx context.Context,
	name, typ string,
	config map[string]string,
	userID int,
) (int, error) {
	if err := validateChannelType(typ); err != nil {
		return 0, err
	}
	if err := validateChannelConfig(typ, config); err != nil {
		return 0, err
	}

	cfgJSON, err := json.Marshal(config)
	if err != nil {
		return 0, fmt.Errorf("marshalling config: %w", err)
	}

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var id int
	err = s.pool.QueryRow(qctx,
		`INSERT INTO sage.notification_channels
		    (name, type, config, created_by)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		name, typ, cfgJSON, userID,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("inserting channel: %w", err)
	}
	return id, nil
}

// ListChannels returns all notification channels.
func (s *NotificationStore) ListChannels(
	ctx context.Context,
) ([]notify.Channel, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := s.pool.Query(qctx,
		`SELECT id, name, type, config, enabled
		 FROM sage.notification_channels ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing channels: %w", err)
	}
	defer rows.Close()

	var result []notify.Channel
	for rows.Next() {
		var ch notify.Channel
		var cfgJSON []byte
		if err := rows.Scan(
			&ch.ID, &ch.Name, &ch.Type, &cfgJSON, &ch.Enabled,
		); err != nil {
			return nil, fmt.Errorf("scanning channel: %w", err)
		}
		ch.Config = parseJSONConfig(cfgJSON)
		result = append(result, ch)
	}
	return result, rows.Err()
}

// GetChannel returns a single channel by ID.
func (s *NotificationStore) GetChannel(
	ctx context.Context, id int,
) (*notify.Channel, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var ch notify.Channel
	var cfgJSON []byte
	err := s.pool.QueryRow(qctx,
		`SELECT id, name, type, config, enabled
		 FROM sage.notification_channels WHERE id = $1`, id,
	).Scan(&ch.ID, &ch.Name, &ch.Type, &cfgJSON, &ch.Enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: channel %d", ErrNotFound, id)
		}
		return nil, fmt.Errorf("getting channel %d: %w", id, err)
	}
	ch.Config = parseJSONConfig(cfgJSON)
	return &ch, nil
}

// UpdateChannel modifies a channel's name, config, and enabled state.
func (s *NotificationStore) UpdateChannel(
	ctx context.Context,
	id int, name string,
	config map[string]string,
	enabled bool,
) error {
	ch, err := s.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	config = preserveMaskedNotificationSecrets(ch.Config, config)
	if err := validateChannelConfig(ch.Type, config); err != nil {
		return err
	}

	cfgJSON, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`UPDATE sage.notification_channels
		 SET name = $1, config = $2, enabled = $3
		 WHERE id = $4`,
		name, cfgJSON, enabled, id)
	if err != nil {
		return fmt.Errorf("updating channel %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: channel %d", ErrNotFound, id)
	}
	return nil
}

func preserveMaskedNotificationSecrets(
	existing, incoming map[string]string,
) map[string]string {
	if incoming == nil {
		incoming = map[string]string{}
	}
	merged := make(map[string]string, len(incoming))
	for key, value := range incoming {
		merged[key] = value
	}
	for _, key := range notificationSecretKeys {
		if isMaskedNotificationSecret(merged[key]) {
			if existingValue, ok := existing[key]; ok {
				merged[key] = existingValue
			}
		}
	}
	return merged
}

func isMaskedNotificationSecret(value string) bool {
	return value == "****" || strings.Contains(value, "****")
}

// DeleteChannel removes a notification channel.
func (s *NotificationStore) DeleteChannel(
	ctx context.Context, id int,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		"DELETE FROM sage.notification_channels WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("deleting channel %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: channel %d", ErrNotFound, id)
	}
	return nil
}

// TestChannel sends a test notification through the channel.
func (s *NotificationStore) TestChannel(
	ctx context.Context, id int,
) error {
	ch, err := s.GetChannel(ctx, id)
	if err != nil {
		return err
	}
	if s.dispatcher == nil {
		return fmt.Errorf("dispatcher not configured")
	}

	testEvt := notify.Event{
		Type:     "action_executed",
		Severity: "info",
		Subject:  "pg_sage test notification",
		Body:     fmt.Sprintf("Test from channel %q", ch.Name),
		Data:     map[string]any{"test": true},
	}

	sender := s.dispatcher
	_ = sender // use dispatcher's registered senders directly
	return sendTestDirect(ctx, s.dispatcher, *ch, testEvt)
}
