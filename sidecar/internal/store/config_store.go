package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/crypto"
)

const configGenerationKey = "__pg_sage_config_generation"

var ErrConfigGenerationConflict = errors.New("config generation conflict")
var ErrConfigDatabaseNotFound = errors.New("config database not found")

// ConfigOverride represents a single config override from sage.config.
type ConfigOverride struct {
	Key        string
	Value      string
	DatabaseID int // 0 = global
	UpdatedAt  time.Time
	UpdatedBy  int
}

// ConfigOverrideWrite is one member of an atomic override revision.
type ConfigOverrideWrite struct {
	Key   string
	Value string
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
	// keyring seals secret keys at rest (CG-01); nil keeps plaintext.
	keyring *crypto.Keyring
}

// GetGeneration returns the durable CAS generation for one config scope.
// Existing installations without a generation row begin at generation one.
func (s *ConfigStore) GetGeneration(
	ctx context.Context, databaseID int,
) (uint64, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var value string
	var err error
	if databaseID == 0 {
		err = s.pool.QueryRow(qctx,
			`/* pg_sage */ SELECT value FROM sage.config
			 WHERE key = $1 AND database_id IS NULL`,
			configGenerationKey,
		).Scan(&value)
	} else {
		err = s.pool.QueryRow(qctx,
			`/* pg_sage */ SELECT value FROM sage.config
			 WHERE key = $1 AND database_id = $2`,
			configGenerationKey, databaseID,
		).Scan(&value)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read config generation: %w", err)
	}
	var generation uint64
	if _, err := fmt.Sscan(value, &generation); err != nil || generation == 0 {
		return 0, fmt.Errorf("invalid durable config generation %q", value)
	}
	return generation, nil
}

// NewConfigStore creates a ConfigStore with the given pool.
func NewConfigStore(pool *pgxpool.Pool) *ConfigStore {
	return &ConfigStore{pool: pool, keyring: defaultConfigKeyring.Load()}
}

// SetOverride upserts a config override. databaseID=0 means global.
// Logs the change to sage.config_audit.
func (s *ConfigStore) SetOverride(
	ctx context.Context, key, value string,
	databaseID int, userID int,
) error {
	return s.SetOverrides(ctx, []ConfigOverrideWrite{{
		Key: key, Value: value,
	}}, databaseID, userID)
}

// SetOverrides validates and persists a complete override revision in one
// transaction, including its audit entries.
func (s *ConfigStore) SetOverrides(
	ctx context.Context, writes []ConfigOverrideWrite,
	databaseID int, userID int,
) error {
	for _, write := range writes {
		if err := validateConfigKey(write.Key); err != nil {
			return err
		}
		if err := validateConfigValue(write.Key, write.Value); err != nil {
			return err
		}
	}
	if len(writes) == 0 {
		return nil
	}

	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tx, err := s.pool.Begin(qctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qctx) }()

	for _, write := range writes {
		if err := s.persistConfigOverride(
			qctx, tx, write, databaseID, userID,
		); err != nil {
			return err
		}
	}

	return tx.Commit(qctx)
}

func deleteConfigOverride(
	ctx context.Context, tx pgx.Tx, key string, databaseID int,
) error {
	var err error
	if databaseID == 0 {
		_, err = tx.Exec(ctx,
			`/* pg_sage */ DELETE FROM sage.config
			 WHERE key = $1 AND database_id IS NULL`, key)
	} else {
		_, err = tx.Exec(ctx,
			`/* pg_sage */ DELETE FROM sage.config
			 WHERE key = $1 AND database_id = $2`, key, databaseID)
	}
	if err != nil {
		return fmt.Errorf("delete config override %q: %w", key, err)
	}
	return nil
}

func (s *ConfigStore) persistConfigOverride(
	ctx context.Context, tx pgx.Tx, write ConfigOverrideWrite,
	databaseID int, userID int,
) error {
	stored, err := s.sealForStorage(write.Key, write.Value, databaseID)
	if err != nil {
		return fmt.Errorf("sealing %s: %w", write.Key, err)
	}
	oldValue, err := getOldValue(ctx, tx, write.Key, databaseID)
	if err != nil {
		return fmt.Errorf("reading old value: %w", err)
	}
	if err := upsertOverride(
		ctx, tx, write.Key, stored, databaseID, userID,
	); err != nil {
		return fmt.Errorf("upserting override: %w", err)
	}
	if err := insertAudit(
		ctx, tx, write.Key, oldValue, write.Value, databaseID, userID,
	); err != nil {
		return fmt.Errorf("inserting audit: %w", err)
	}
	return nil
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

	switch databaseID {
	case -1:
		query = `SELECT key, value,
			COALESCE(database_id, 0),
			updated_at, COALESCE(updated_by_user_id, 0)
			FROM sage.config
			WHERE key != 'trust_ramp_start'
			AND key != '` + configGenerationKey + `'
			ORDER BY COALESCE(database_id, 0), key`
	case 0:
		query = `SELECT key, value,
			COALESCE(database_id, 0),
			updated_at, COALESCE(updated_by_user_id, 0)
			FROM sage.config
			WHERE database_id IS NULL
			AND key != 'trust_ramp_start'
			AND key != '` + configGenerationKey + `'
			ORDER BY key`
	default:
		query = `SELECT key, value,
			COALESCE(database_id, 0),
			updated_at, COALESCE(updated_by_user_id, 0)
			FROM sage.config
			WHERE database_id = $1
			AND key != '` + configGenerationKey + `'
			ORDER BY key`
		args = append(args, databaseID)
	}

	rows, err := s.pool.Query(qctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying overrides: %w", err)
	}
	defer rows.Close()

	overrides, err := scanOverrideRows(rows)
	if err != nil {
		return nil, err
	}
	return s.openSecrets(overrides), nil
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
			`/* pg_sage */ DELETE FROM sage.config
			 WHERE key = $1 AND database_id IS NULL`,
			key)
	} else {
		_, err = s.pool.Exec(qctx,
			`/* pg_sage */ DELETE FROM sage.config
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
		`/* pg_sage */ SELECT id, key, COALESCE(old_value, ''),
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
