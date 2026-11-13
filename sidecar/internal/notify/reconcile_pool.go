package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/crypto"
)

var _ ReconcileStore = (*PoolStore)(nil)

// reconcileTimeout bounds one reconcile transaction, lock wait included.
const reconcileTimeout = 30 * time.Second

// InReconcileTx runs fn in one transaction on the control database under
// a transaction-scoped advisory lock, so two sidecars sharing it apply the
// block one after the other and the second finds nothing to do.
func (s *PoolStore) InReconcileTx(
	ctx context.Context, fn func(ReconcileTx) error,
) error {
	if s.pool == nil {
		return errors.New("notification pool is nil")
	}
	qctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()
	tx, err := s.pool.Begin(qctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(qctx, `/* pg_sage */ SET LOCAL lock_timeout = '5s'`); err != nil {
		return fmt.Errorf("set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(qctx, `/* pg_sage */ SELECT pg_advisory_xact_lock(
		hashtextextended('pg_sage|notify|reconcile', 0))`); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	if err := fn(poolReconcileTx{tx: tx, keyring: s.keyring}); err != nil {
		return err
	}
	if err := tx.Commit(qctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

type poolReconcileTx struct {
	tx      pgx.Tx
	keyring *crypto.Keyring
}

func (t poolReconcileTx) ManagedChannels(ctx context.Context) ([]ManagedChannel, error) {
	rows, err := t.tx.Query(ctx, `/* pg_sage */ SELECT id, name, type,
		COALESCE(config_hash, '') FROM sage.notification_channels
		WHERE managed_by = 'config' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ManagedChannel, error) {
		var ch ManagedChannel
		err := row.Scan(&ch.ID, &ch.Name, &ch.Type, &ch.Hash)
		return ch, err
	})
}

func (t poolReconcileTx) ManagedRules(ctx context.Context) ([]ManagedRule, error) {
	rows, err := t.tx.Query(ctx, `/* pg_sage */ SELECT id, channel_id, event,
		COALESCE(min_severity, ''), COALESCE(config_hash, '')
		FROM sage.notification_rules WHERE managed_by = 'config' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ManagedRule, error) {
		var r ManagedRule
		err := row.Scan(&r.ID, &r.ChannelID, &r.Event, &r.MinSeverity, &r.Hash)
		return r, err
	})
}

// sealedConfig seals ch's secrets for channel id, as the API does.
func (t poolReconcileTx) sealedConfig(ch ManagedChannel, id int) ([]byte, error) {
	cfg, err := SealSecrets(ch.Config, t.keyring, id)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cfg)
}

func (t poolReconcileTx) InsertChannel(ctx context.Context, ch ManagedChannel) (int, error) {
	var id int
	if err := t.tx.QueryRow(ctx, `/* pg_sage */ SELECT
		nextval(pg_get_serial_sequence('sage.notification_channels', 'id'))::int`,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("allocating channel id: %w", err)
	}
	cfg, err := t.sealedConfig(ch, id)
	if err != nil {
		return 0, err
	}
	_, err = t.tx.Exec(ctx, `/* pg_sage */ INSERT INTO sage.notification_channels
		(id, name, type, config, enabled, managed_by, config_hash)
		VALUES ($1, $2, $3, $4, true, 'config', $5)`, id, ch.Name, ch.Type, cfg, ch.Hash)
	return id, err
}

func (t poolReconcileTx) UpdateChannel(ctx context.Context, ch ManagedChannel) error {
	cfg, err := t.sealedConfig(ch, ch.ID)
	if err != nil {
		return err
	}
	tag, err := t.tx.Exec(ctx, `/* pg_sage */ UPDATE sage.notification_channels
		SET type = $2, config = $3, enabled = true, config_hash = $4
		WHERE id = $1 AND managed_by = 'config'`, ch.ID, ch.Type, cfg, ch.Hash)
	if err == nil && tag.RowsAffected() == 0 {
		err = fmt.Errorf("config channel %d not found", ch.ID)
	}
	return err
}

func (t poolReconcileTx) DeleteChannel(ctx context.Context, ch ManagedChannel) error {
	_, err := t.tx.Exec(ctx, `/* pg_sage */ DELETE FROM sage.notification_channels
		WHERE id = $1 AND managed_by = 'config'`, ch.ID)
	return err
}

func (t poolReconcileTx) InsertRule(ctx context.Context, r ManagedRule) (int, error) {
	var id int
	err := t.tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.notification_rules
		(channel_id, event, min_severity, enabled, managed_by, config_hash)
		VALUES ($1, $2, $3, true, 'config', $4) RETURNING id`,
		r.ChannelID, r.Event, r.MinSeverity, r.Hash).Scan(&id)
	return id, err
}

func (t poolReconcileTx) DeleteRule(ctx context.Context, r ManagedRule) error {
	_, err := t.tx.Exec(ctx, `/* pg_sage */ DELETE FROM sage.notification_rules
		WHERE id = $1 AND managed_by = 'config'`, r.ID)
	return err
}

// ChannelByName loads one channel by name with its secrets opened; the
// legacy cutover reads the reconciled PagerDuty channel with it.
func (s *PoolStore) ChannelByName(ctx context.Context, name string) (*Channel, error) {
	if s.pool == nil {
		return nil, fmt.Errorf("get channel %q: notification pool is nil", name)
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var ch Channel
	var cfgJSON []byte
	err := s.pool.QueryRow(qctx, `/* pg_sage */ SELECT id, name, type, config, enabled,
		managed_by FROM sage.notification_channels WHERE name = $1`, name,
	).Scan(&ch.ID, &ch.Name, &ch.Type, &cfgJSON, &ch.Enabled, &ch.ManagedBy)
	if err != nil {
		return nil, fmt.Errorf("get channel %q: %w", name, err)
	}
	cfg, err := parseConfig(cfgJSON)
	if err != nil {
		return nil, fmt.Errorf("channel %q: %w", name, err)
	}
	if ch.Config, err = OpenSecrets(cfg, s.keyring, ch.ID); err != nil {
		return nil, fmt.Errorf("channel %q: %w", name, err)
	}
	return &ch, nil
}
