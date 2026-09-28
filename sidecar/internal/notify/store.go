package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
}

// NewPoolStore creates a PoolStore. secretKey (32 bytes, may be nil)
// decrypts channel secrets sealed by the notification API (G7-B20).
func NewPoolStore(pool *pgxpool.Pool, secretKey []byte) *PoolStore {
	return &PoolStore{pool: pool, secretKey: secretKey}
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
		`/* pg_sage */ SELECT id, name, type, config, enabled
		 FROM sage.notification_channels
		 WHERE id = $1`, id,
	).Scan(&ch.ID, &ch.Name, &ch.Type, &cfgJSON, &ch.Enabled)
	if err != nil {
		return nil, fmt.Errorf("get channel %d: %w", id, err)
	}
	cfg, err := parseConfig(cfgJSON)
	if err != nil {
		return nil, fmt.Errorf("channel %d: %w", id, err)
	}
	if ch.Config, err = OpenSecrets(cfg, s.secretKey); err != nil {
		return nil, fmt.Errorf("channel %d: %w", id, err)
	}
	return &ch, nil
}

// LogDelivery records one delivery attempt. errMsg must already be
// redacted; it is redacted again here as defense in depth.
func (s *PoolStore) LogDelivery(
	ctx context.Context, channelID int, event Event, status, errMsg string,
) error {
	if s.pool == nil {
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.pool.Exec(qctx,
		`/* pg_sage */ INSERT INTO sage.notification_log
		    (channel_id, event, subject, body, status, error)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		channelID, event.Type, event.Subject,
		event.Body, status, RedactURLs(errMsg))
	if err != nil {
		return fmt.Errorf("insert notification_log: %w", err)
	}
	return nil
}

var _ RuleStore = (*PoolStore)(nil)
