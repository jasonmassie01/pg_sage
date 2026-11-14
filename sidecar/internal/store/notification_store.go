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
	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/notify"
)

// NotificationStore handles CRUD for notification channels, rules,
// and log entries.
type NotificationStore struct {
	pool       *pgxpool.Pool
	dispatcher *notify.Dispatcher
	secretKey  []byte
	keyring    *crypto.Keyring
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
	s.keyring = notify.SecretKeyring(key, nil)
	return s
}

// WithPreviousSecretKey also opens channel secrets sealed under the
// previous key of a rotation, so ResealChannelSecrets can move them to the
// active key (security-08). Call it after WithSecretKey. Without an active
// key, or with an unusable previous key, the keyring is unchanged.
func (s *NotificationStore) WithPreviousSecretKey(previous []byte) *NotificationStore {
	if s.keyring == nil || len(previous) == 0 {
		return s
	}
	if kr := notify.SecretKeyring(s.secretKey, previous); kr != nil {
		s.keyring = kr
	}
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
	// Database is the database the delivery was for; "unknown" for a
	// fleet-wide event or a row written before alerting-06c.
	Database string `json:"database"`
	// DedupKey is the event's dedup key; empty for rows before alerting-06c.
	DedupKey string `json:"dedup_key"`
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
	if err := validateAPIChannelName(name); err != nil {
		return 0, err
	}
	if err := s.validateChannel(typ, config); err != nil {
		return 0, err
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// The id comes first so sealed secrets can be bound to the channel.
	var id int
	if err := s.pool.QueryRow(qctx, `/* pg_sage */ SELECT
		nextval(pg_get_serial_sequence('sage.notification_channels', 'id'))::int`,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("allocating channel id: %w", err)
	}
	cfgJSON, err := s.encodeConfig(config, id)
	if err != nil {
		return 0, err
	}
	_, err = s.pool.Exec(qctx,
		`/* pg_sage */ INSERT INTO sage.notification_channels
		    (id, name, type, config, created_by)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, name, typ, cfgJSON, userID,
	)
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

// encodeConfig seals secrets for channel id (when a key is configured)
// and marshals.
func (s *NotificationStore) encodeConfig(config map[string]string, id int) ([]byte, error) {
	config, err := notify.SealSecrets(config, s.keyring, id)
	if err != nil {
		return nil, err
	}
	cfgJSON, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshalling config: %w", err)
	}
	return cfgJSON, nil
}

func (s *NotificationStore) decodeConfig(
	cfgJSON []byte, id int,
) (map[string]string, error) {
	return notify.OpenSecrets(parseJSONConfig(cfgJSON), s.keyring, id)
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
		`/* pg_sage */ SELECT id, name, type, config, enabled, managed_by
		 FROM sage.notification_channels WHERE id = $1`, id,
	).Scan(&ch.ID, &ch.Name, &ch.Type, &cfgJSON, &ch.Enabled, &ch.ManagedBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: channel %d", ErrNotFound, id)
		}
		return nil, fmt.Errorf("getting channel %d: %w", id, err)
	}
	stored := parseJSONConfig(cfgJSON)
	if ch.Config, err = notify.OpenSecrets(stored, s.keyring, id); err != nil {
		return nil, fmt.Errorf("channel %d: %w", id, err)
	}
	if notify.NeedsReseal(stored, s.keyring) {
		// A compare-and-swap: a channel updated since the read keeps its
		// update; losing the swap leaves the newer value for the next read.
		row := storedChannelConfig{id: id, raw: cfgJSON}
		if _, err := s.swapChannelConfig(qctx, row, ch.Config); err != nil {
			return nil, fmt.Errorf("migrating channel %d secrets: %w", id, err)
		}
	}
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
	if ch.ManagedBy == notify.ManagedByConfig {
		return fmt.Errorf("channel %d: %w", id, ErrManagedByConfig)
	}
	if name != ch.Name {
		if err := validateAPIChannelName(name); err != nil {
			return err
		}
	}
	if err := checkSMTPDestinationSecrets(ch.Config, config); err != nil {
		return err
	}
	if err := checkDestinationSecrets(ch.Type, ch.Config, config); err != nil {
		return err
	}
	config = preserveMaskedNotificationSecrets(ch.Config, config)
	if err := s.validateChannel(ch.Type, config); err != nil {
		return err
	}
	cfgJSON, err := s.encodeConfig(config, id)
	if err != nil {
		return err
	}

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tag, err := s.pool.Exec(qctx,
		`/* pg_sage */ UPDATE sage.notification_channels
		 SET name = $1, config = $2, enabled = $3
		 WHERE id = $4 AND managed_by = 'api'`,
		name, cfgJSON, enabled, id)
	if err != nil {
		return fmt.Errorf("updating channel %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return s.channelNotWritten(ctx, id)
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
		`/* pg_sage */ DELETE FROM sage.notification_channels
		 WHERE id = $1 AND managed_by = 'api'`, id)
	if err != nil {
		return fmt.Errorf("deleting channel %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return s.channelNotWritten(ctx, id)
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
	if notify.IsTicketIntegration(ch.Type) {
		return fmt.Errorf("%w: a %s integration is checked when a ticket is created "+
			"from a case", ErrValidation, ch.Type)
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
