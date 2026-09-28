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
	secretKey  []byte
	policy     notify.TargetPolicy
}

// NewNotificationStore creates a NotificationStore. Channel targets are
// validated with the strict notify.TargetPolicy (no internal hosts).
func NewNotificationStore(
	pool *pgxpool.Pool,
	dispatcher *notify.Dispatcher,
) *NotificationStore {
	return &NotificationStore{
		pool:       pool,
		dispatcher: dispatcher,
	}
}

// WithSecretKey enables encryption of channel secrets at rest with a
// 32-byte key (the meta-DB encryption key). Existing plaintext rows are
// migrated lazily when read (G7-B20). Every dispatcher reading these
// channels must use notify.NewPoolStore with the same key.
func (s *NotificationStore) WithSecretKey(key []byte) *NotificationStore {
	s.secretKey = key
	return s
}

// WithTargetPolicy overrides the channel target policy (e.g. to allow an
// internal SMTP relay).
func (s *NotificationStore) WithTargetPolicy(p notify.TargetPolicy) *NotificationStore {
	s.policy = p
	return s
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
	if err := s.validateChannel(typ, config); err != nil {
		return 0, err
	}
	cfgJSON, err := s.encodeConfig(config)
	if err != nil {
		return 0, err
	}

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var id int
	err = s.pool.QueryRow(qctx,
		`/* pg_sage */ INSERT INTO sage.notification_channels
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

func (s *NotificationStore) validateChannel(
	typ string, config map[string]string,
) error {
	if err := validateChannelConfig(typ, config); err != nil {
		return err
	}
	return validateChannelTargets(s.policy, typ, config)
}

// encodeConfig seals secrets (when a key is configured) and marshals.
func (s *NotificationStore) encodeConfig(config map[string]string) ([]byte, error) {
	if len(s.secretKey) > 0 {
		sealed, err := notify.SealSecrets(config, s.secretKey)
		if err != nil {
			return nil, err
		}
		config = sealed
	}
	cfgJSON, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshalling config: %w", err)
	}
	return cfgJSON, nil
}

// ListChannels returns all notification channels.
func (s *NotificationStore) ListChannels(
	ctx context.Context,
) ([]notify.Channel, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := s.pool.Query(qctx,
		`/* pg_sage */ SELECT id, name, type, config, enabled
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
		if ch.Config, err = s.decodeConfig(cfgJSON); err != nil {
			return nil, fmt.Errorf("channel %d: %w", ch.ID, err)
		}
		result = append(result, ch)
	}
	return result, rows.Err()
}

func (s *NotificationStore) decodeConfig(cfgJSON []byte) (map[string]string, error) {
	return notify.OpenSecrets(parseJSONConfig(cfgJSON), s.secretKey)
}

// GetChannel returns a single channel by ID, migrating plaintext
// secrets to sealed form when a key is configured.
func (s *NotificationStore) GetChannel(
	ctx context.Context, id int,
) (*notify.Channel, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var ch notify.Channel
	var cfgJSON []byte
	err := s.pool.QueryRow(qctx,
		`/* pg_sage */ SELECT id, name, type, config, enabled
		 FROM sage.notification_channels WHERE id = $1`, id,
	).Scan(&ch.ID, &ch.Name, &ch.Type, &cfgJSON, &ch.Enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: channel %d", ErrNotFound, id)
		}
		return nil, fmt.Errorf("getting channel %d: %w", id, err)
	}
	stored := parseJSONConfig(cfgJSON)
	if ch.Config, err = notify.OpenSecrets(stored, s.secretKey); err != nil {
		return nil, fmt.Errorf("channel %d: %w", id, err)
	}
	if len(s.secretKey) > 0 && notify.HasPlaintextSecrets(stored) {
		if err := s.writeConfig(qctx, id, ch.Config); err != nil {
			return nil, fmt.Errorf("migrating channel %d secrets: %w", id, err)
		}
	}
	return &ch, nil
}

func (s *NotificationStore) writeConfig(
	ctx context.Context, id int, config map[string]string,
) error {
	cfgJSON, err := s.encodeConfig(config)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`/* pg_sage */ UPDATE sage.notification_channels
		 SET config = $1 WHERE id = $2`, cfgJSON, id)
	return err
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
	if err := s.validateChannel(ch.Type, config); err != nil {
		return err
	}
	cfgJSON, err := s.encodeConfig(config)
	if err != nil {
		return err
	}

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.notification_channels
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

// ErrTestDeliveryFailed is returned by TestChannel when the send fails.
// The detailed (redacted) cause is in sage.notification_log; it is not
// echoed to the API caller so the endpoint is not a port-scan or
// response oracle (G7-B21).
var ErrTestDeliveryFailed = errors.New(
	"test notification delivery failed; see the notification log")

// TestChannel sends a test notification through the channel. Test
// sends are logged with event type "test" (G7-B37).
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
		Type:     "test",
		Severity: "info",
		Subject:  "pg_sage test notification",
		Body:     fmt.Sprintf("Test from channel %q", ch.Name),
		Data:     map[string]any{"test": true},
	}
	if err := s.dispatcher.SendDirect(ctx, *ch, testEvt); err != nil {
		return ErrTestDeliveryFailed
	}
	return nil
}
