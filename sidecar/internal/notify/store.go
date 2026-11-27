package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/crypto"
)

// RuleStore is where the dispatcher reads rules and channels and
// records deliveries. In fleet / meta-DB mode it must be backed by the
// control-plane pool that the UI writes to, not by each monitored
// database's pool (G7-B05).
type RuleStore interface {
	MatchingRules(ctx context.Context, eventType string) ([]Rule, error)
	Channel(ctx context.Context, id int) (*Channel, error)
	LogDelivery(ctx context.Context, channelID int, event Event,
		status, errMsg string) error
}

// PoolStore implements RuleStore on sage.notification_* tables.
type PoolStore struct {
	pool      *pgxpool.Pool
	secretKey []byte
	keyring   *crypto.Keyring
}

// NewPoolStore creates a PoolStore. secretKey (32 bytes, may be nil)
// decrypts channel secrets sealed by the notification API (G7-B20).
func NewPoolStore(pool *pgxpool.Pool, secretKey []byte) *PoolStore {
	return &PoolStore{pool: pool, secretKey: secretKey, keyring: SecretKeyring(secretKey, nil)}
}

// WithPreviousKey also opens channel secrets sealed under the previous key
// of a rotation (security-08); secretKey stays the active key. Without an
// active key, or with an unusable previous key, the keyring is unchanged.
func (s *PoolStore) WithPreviousKey(previous []byte) *PoolStore {
	if s.keyring == nil || len(previous) == 0 {
		return s
	}
	if kr := SecretKeyring(s.secretKey, previous); kr != nil {
		s.keyring = kr
	}
	return s
}

// SecretKeyring is the channel-secret keyring for a 32-byte active key and
// an optional previous key; nil when active is not a usable key, so
// secrets stay plaintext as before.
func SecretKeyring(active, previous []byte) *crypto.Keyring {
	if len(active) != 32 {
		return nil
	}
	// NewKeyring only fails on a key that is not 32 bytes.
	kr, err := crypto.NewKeyring(active, previous)
	if err != nil {
		return nil
	}
	return kr
}

// MatchingRules returns enabled rules for an event type.
func (s *PoolStore) MatchingRules(
	ctx context.Context, eventType string,
) ([]Rule, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("query rules: notification pool is nil")
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx,
		`/* pg_sage */ SELECT id, channel_id, event, min_severity
		 FROM sage.notification_rules
		 WHERE event = $1 AND enabled = true`, eventType)
	if err != nil {
		return nil, fmt.Errorf("query rules: %w", err)
	}
	defer rows.Close()
	var rules []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.ChannelID, &r.Event, &r.MinSeverity); err != nil {
			return nil, fmt.Errorf("scan rule: %w", err)
		}
		r.Enabled = true
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

// Channel loads one channel with its secrets decrypted.
func (s *PoolStore) Channel(ctx context.Context, id int) (*Channel, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("get channel %d: notification pool is nil", id)
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var ch Channel
	var cfgJSON []byte
	err := s.pool.QueryRow(qctx,
		`/* pg_sage */ SELECT id, name, type, config, enabled, COALESCE(quiet_start, ''),
		        COALESCE(quiet_end, ''), COALESCE(quiet_tz, '')
		 FROM sage.notification_channels
		 WHERE id = $1`, id,
	).Scan(&ch.ID, &ch.Name, &ch.Type, &cfgJSON, &ch.Enabled, &ch.QuietStart, &ch.QuietEnd,
		&ch.QuietTZ)
	if err != nil {
		return nil, fmt.Errorf("get channel %d: %w", id, err)
	}
	cfg, err := parseConfig(cfgJSON)
	if err != nil {
		return nil, fmt.Errorf("channel %d: %w", id, err)
	}
	if ch.Config, err = OpenSecrets(cfg, s.keyring, id); err != nil {
		return nil, fmt.Errorf("channel %d: %w", id, err)
	}
	return &ch, nil
}

// LogDelivery records one delivery attempt. errMsg must already be
// redacted; it is redacted again here as defense in depth.
func (s *PoolStore) LogDelivery(
	ctx context.Context, channelID int, event Event, status, errMsg string,
) error {
	return s.RecordDelivery(ctx, Delivery{ChannelID: channelID, Event: event,
		Status: status, Error: errMsg})
}

var _ RuleStore = (*PoolStore)(nil)
