package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// ConfigOverride represents a single config override from sage.config.
type ConfigOverride struct {
	Key        string
	Value      string
	DatabaseID int // 0 = global
	UpdatedAt  time.Time
	UpdatedBy  int
}

// ConfigAuditEntry represents a row from sage.config_audit.
type ConfigAuditEntry struct {
	ID         int
	Key        string
	OldValue   string
	NewValue   string
	DatabaseID int
	ChangedBy  int
	ChangedAt  time.Time
}

// ConfigStore handles CRUD for sage.config overrides and
// sage.config_audit.
type ConfigStore struct {
	pool *pgxpool.Pool
}

// NewConfigStore creates a ConfigStore with the given pool.
func NewConfigStore(pool *pgxpool.Pool) *ConfigStore {
	return &ConfigStore{pool: pool}
}

// SetOverride upserts a config override. databaseID=0 means global.
// Logs the change to sage.config_audit.
func (s *ConfigStore) SetOverride(
	ctx context.Context, key, value string,
	databaseID int, userID int,
) error {
	if err := validateConfigKey(key); err != nil {
		return err
	}
	if err := validateConfigValue(key, value); err != nil {
		return err
	}

	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tx, err := s.pool.Begin(qctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(qctx)

	oldValue, err := getOldValue(qctx, tx, key, databaseID)
	if err != nil {
		return fmt.Errorf("reading old value: %w", err)
	}

	if err := upsertOverride(
		qctx, tx, key, value, databaseID, userID,
	); err != nil {
		return fmt.Errorf("upserting override: %w", err)
	}

	if err := insertAudit(
		qctx, tx, key, oldValue, value, databaseID, userID,
	); err != nil {
		return fmt.Errorf("inserting audit: %w", err)
	}

	return tx.Commit(qctx)
}

// GetOverrides returns all overrides. databaseID=0 returns global
// overrides, databaseID=-1 returns all overrides.
func (s *ConfigStore) GetOverrides(
	ctx context.Context, databaseID int,
) ([]ConfigOverride, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var query string
	var args []any

	switch {
	case databaseID == -1:
		query = `SELECT key, value,
			COALESCE(database_id, 0),
			updated_at, COALESCE(updated_by_user_id, 0)
			FROM sage.config
			WHERE key != 'trust_ramp_start'
			ORDER BY COALESCE(database_id, 0), key`
	case databaseID == 0:
		query = `SELECT key, value,
			COALESCE(database_id, 0),
			updated_at, COALESCE(updated_by_user_id, 0)
			FROM sage.config
			WHERE database_id IS NULL
			AND key != 'trust_ramp_start'
			ORDER BY key`
	default:
		query = `SELECT key, value,
			COALESCE(database_id, 0),
			updated_at, COALESCE(updated_by_user_id, 0)
			FROM sage.config
			WHERE database_id = $1
			ORDER BY key`
		args = append(args, databaseID)
	}

	rows, err := s.pool.Query(qctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying overrides: %w", err)
	}
	defer rows.Close()

	return scanOverrideRows(rows)
}

// DeleteOverride removes a specific override. databaseID=0 means
// global.
func (s *ConfigStore) DeleteOverride(
	ctx context.Context, key string, databaseID int,
) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var err error
	if databaseID == 0 {
		_, err = s.pool.Exec(qctx,
			`DELETE FROM sage.config
			 WHERE key = $1 AND database_id IS NULL`,
			key)
	} else {
		_, err = s.pool.Exec(qctx,
			`DELETE FROM sage.config
			 WHERE key = $1 AND database_id = $2`,
			key, databaseID)
	}
	if err != nil {
		return fmt.Errorf("deleting override %q: %w", key, err)
	}
	return nil
}

// GetMergedConfig returns the effective config for a database,
// merging: defaults < YAML < global overrides < per-DB overrides.
func (s *ConfigStore) GetMergedConfig(
	ctx context.Context, cfg *config.Config, databaseID int,
) (map[string]any, error) {
	merged := configToMap(cfg)

	globalOverrides, err := s.GetOverrides(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("global overrides: %w", err)
	}
	applyOverrides(merged, globalOverrides, "override")

	if databaseID > 0 {
		dbOverrides, err := s.GetOverrides(ctx, databaseID)
		if err != nil {
			return nil, fmt.Errorf("db overrides: %w", err)
		}
		applyOverrides(merged, dbOverrides, "db_override")
	}

	return merged, nil
}

// GetAuditLog returns recent config change audit entries.
func (s *ConfigStore) GetAuditLog(
	ctx context.Context, limit int,
) ([]ConfigAuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := s.pool.Query(qctx,
		`SELECT id, key, COALESCE(old_value, ''),
			new_value, COALESCE(database_id, 0),
			COALESCE(changed_by, 0), changed_at
		 FROM sage.config_audit
		 ORDER BY changed_at DESC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("querying audit log: %w", err)
	}
	defer rows.Close()

	return scanAuditRows(rows)
}
