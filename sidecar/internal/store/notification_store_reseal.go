package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/notify"
)

type storedChannelConfig struct {
	id  int
	raw []byte
}

// ResealChannelSecrets seals plaintext and legacy enc:v1 channel secrets
// and re-seals secrets sealed under a previous key, all to sage-enc:v1
// under the active key bound to the channel (security-08). Each write is a
// compare-and-swap on the stored config, so a channel edited concurrently
// keeps its edit. Without a key it only counts channels with plaintext
// secrets.
func (s *NotificationStore) ResealChannelSecrets(ctx context.Context) (SecretSealReport, error) {
	var report SecretSealReport
	rows, err := s.channelConfigs(ctx)
	if err != nil {
		return report, err
	}
	for _, row := range rows {
		stored := parseJSONConfig(row.raw)
		if s.keyring == nil {
			if notify.HasPlaintextSecrets(stored) {
				report.Plaintext++
			}
			continue
		}
		if !notify.NeedsReseal(stored, s.keyring) {
			continue
		}
		opened, err := notify.OpenSecrets(stored, s.keyring, row.id)
		if err != nil {
			slog.Error("channel secrets cannot be decrypted; left as stored",
				"channel_id", row.id, "error", err)
			report.Unreadable++
			continue
		}
		swapped, err := s.swapChannelConfig(ctx, row, opened)
		if err != nil {
			return report, err
		}
		if swapped && hasUnboundSecret(stored) {
			report.Sealed++
		} else if swapped {
			report.Rotated++
		}
	}
	return report, nil
}

// hasUnboundSecret reports whether a stored secret is plaintext or a
// legacy enc:v1 value: re-sealing it seals it to its channel (Sealed), as
// opposed to moving a sage-enc:v1 value to the active key (Rotated).
func hasUnboundSecret(cfg map[string]string) bool {
	for _, k := range notify.SecretConfigKeys {
		if v := cfg[k]; v != "" && !crypto.IsSealed(v) {
			return true
		}
	}
	return false
}

// swapChannelConfig writes opened, sealed under the active key, only if
// the stored config is still row.raw.
func (s *NotificationStore) swapChannelConfig(
	ctx context.Context, row storedChannelConfig, opened map[string]string,
) (bool, error) {
	next, err := s.encodeConfig(opened, row.id)
	if err != nil {
		return false, err
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(qctx, `/* pg_sage */ UPDATE sage.notification_channels
		 SET config = $1 WHERE id = $2 AND config = $3::jsonb`,
		next, row.id, row.raw)
	if err != nil {
		return false, fmt.Errorf("re-sealing channel %d secrets: %w", row.id, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *NotificationStore) channelConfigs(ctx context.Context) ([]storedChannelConfig, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT id, config
		 FROM sage.notification_channels ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing notification channels: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (storedChannelConfig, error) {
		var c storedChannelConfig
		err := r.Scan(&c.id, &c.raw)
		return c, err
	})
}
