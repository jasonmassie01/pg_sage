package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
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
	return &ConfigStore{pool: pool}
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
		if err := persistConfigOverride(
			qctx, tx, write, databaseID, userID,
		); err != nil {
			return err
		}
	}

	return tx.Commit(qctx)
}

// SetOverridesCAS persists an entire override revision and advances the
// scope generation in the same transaction.
func (s *ConfigStore) SetOverridesCAS(
	ctx context.Context, writes []ConfigOverrideWrite,
	databaseID int, userID int, expected uint64,
) (uint64, error) {
	for _, write := range writes {
		if err := validateConfigKey(write.Key); err != nil {
			return 0, err
		}
		if err := validateConfigValue(write.Key, write.Value); err != nil {
			return 0, err
		}
	}
	if expected == 0 {
		return 0, fmt.Errorf("expected generation must be positive")
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(qctx)
	if err != nil {
		return 0, fmt.Errorf("begin config revision: %w", err)
	}
	defer func() { _ = tx.Rollback(qctx) }()
	current, err := lockConfigGeneration(qctx, tx, databaseID)
	if err != nil {
		return 0, err
	}
	if current != expected {
		return 0, fmt.Errorf("%w: expected %d, durable %d",
			ErrConfigGenerationConflict, expected, current)
	}
	for _, write := range writes {
		if err := persistConfigOverride(
			qctx, tx, write, databaseID, userID,
		); err != nil {
			return 0, err
		}
	}
	next := current + 1
	if err := persistConfigGeneration(
		qctx, tx, databaseID, userID, next,
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(qctx); err != nil {
		return 0, fmt.Errorf("commit config revision: %w", err)
	}
	return next, nil
}

// SetDatabaseOverridesCAS atomically persists database overrides and the
// execution mode row under the database-scoped generation predicate.
func (s *ConfigStore) SetDatabaseOverridesCAS(
	ctx context.Context, writes []ConfigOverrideWrite,
	databaseID int, userID int, expected uint64,
	executionMode *string,
) (uint64, error) {
	if databaseID < 1 {
		return 0, fmt.Errorf("database id must be positive")
	}
	for _, write := range writes {
		if err := validateConfigKey(write.Key); err != nil {
			return 0, err
		}
		if err := validateConfigValue(write.Key, write.Value); err != nil {
			return 0, err
		}
	}
	if expected == 0 {
		return 0, fmt.Errorf("expected generation must be positive")
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(qctx)
	if err != nil {
		return 0, fmt.Errorf("begin database config revision: %w", err)
	}
	defer func() { _ = tx.Rollback(qctx) }()
	current, err := lockConfigGeneration(qctx, tx, databaseID)
	if err != nil {
		return 0, err
	}
	if current != expected {
		return 0, fmt.Errorf("%w: expected %d, durable %d",
			ErrConfigGenerationConflict, expected, current)
	}
	var trustLevel *string
	for i := range writes {
		if writes[i].Key == "trust.level" {
			trustLevel = &writes[i].Value
		}
	}
	if executionMode != nil || trustLevel != nil {
		var oldTrust string
		var oldExecutionMode string
		err := tx.QueryRow(qctx,
			`/* pg_sage */ SELECT trust_level, execution_mode FROM sage.databases
			 WHERE id = $1 FOR UPDATE`, databaseID,
		).Scan(&oldTrust, &oldExecutionMode)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("%w: id %d",
				ErrConfigDatabaseNotFound, databaseID)
		}
		if err != nil {
			return 0, fmt.Errorf("lock database policy: %w", err)
		}
		_, err = tx.Exec(qctx,
			`/* pg_sage */ UPDATE sage.databases SET
			 execution_mode = COALESCE($1, execution_mode),
			 trust_level = COALESCE($2, trust_level)
			 WHERE id = $3`, executionMode, trustLevel, databaseID,
		)
		if err != nil {
			return 0, fmt.Errorf("update database policy: %w", err)
		}
		if executionMode != nil {
			if err := insertAudit(qctx, tx, "execution_mode",
				oldExecutionMode, *executionMode, databaseID, userID); err != nil {
				return 0, fmt.Errorf("audit database execution mode: %w", err)
			}
		}
		if trustLevel != nil {
			if err := insertAudit(qctx, tx, "trust.level",
				oldTrust, *trustLevel, databaseID, userID); err != nil {
				return 0, fmt.Errorf("audit database trust policy: %w", err)
			}
			// Remove legacy higher-priority rows so the canonical database
			// policy cannot be shadowed after restart or reconnect.
			if err := deleteConfigOverride(
				qctx, tx, "trust.level", databaseID,
			); err != nil {
				return 0, err
			}
		}
	}
	for _, write := range writes {
		if write.Key == "trust.level" {
			continue
		}
		if err := persistConfigOverride(
			qctx, tx, write, databaseID, userID,
		); err != nil {
			return 0, err
		}
	}
	next := current + 1
	if err := persistConfigGeneration(
		qctx, tx, databaseID, userID, next,
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(qctx); err != nil {
		return 0, fmt.Errorf("commit database config revision: %w", err)
	}
	return next, nil
}

// DeleteOverrideCAS removes an override and advances the generation in one
// transaction so stale writers cannot overwrite the reset.
func (s *ConfigStore) DeleteOverrideCAS(
	ctx context.Context, key string, databaseID int,
	userID int, expected uint64,
) (uint64, error) {
	if err := validateConfigKey(key); err != nil {
		return 0, err
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(qctx)
	if err != nil {
		return 0, fmt.Errorf("begin config reset: %w", err)
	}
	defer func() { _ = tx.Rollback(qctx) }()
	current, err := lockConfigGeneration(qctx, tx, databaseID)
	if err != nil {
		return 0, err
	}
	if current != expected {
		return 0, fmt.Errorf("%w: expected %d, durable %d",
			ErrConfigGenerationConflict, expected, current)
	}
	oldValue, err := getOldValue(qctx, tx, key, databaseID)
	if err != nil {
		return 0, fmt.Errorf("read reset value: %w", err)
	}
	if err := deleteConfigOverride(qctx, tx, key, databaseID); err != nil {
		return 0, err
	}
	if oldValue != "" {
		if err := insertAudit(
			qctx, tx, key, oldValue, "", databaseID, userID,
		); err != nil {
			return 0, fmt.Errorf("audit config reset: %w", err)
		}
	}
	next := current + 1
	if err := persistConfigGeneration(
		qctx, tx, databaseID, userID, next,
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(qctx); err != nil {
		return 0, fmt.Errorf("commit config reset: %w", err)
	}
	return next, nil
}

func lockConfigGeneration(
	ctx context.Context, tx pgx.Tx, databaseID int,
) (uint64, error) {
	if _, err := tx.Exec(ctx,
		`/* pg_sage */ SELECT pg_advisory_xact_lock($1, $2)`,
		int32(0x53414745), int32(databaseID),
	); err != nil {
		return 0, fmt.Errorf("lock config generation: %w", err)
	}
	var value string
	var err error
	if databaseID == 0 {
		err = tx.QueryRow(ctx,
			`/* pg_sage */ SELECT value FROM sage.config
			 WHERE key = $1 AND database_id IS NULL FOR UPDATE`,
			configGenerationKey,
		).Scan(&value)
	} else {
		err = tx.QueryRow(ctx,
			`/* pg_sage */ SELECT value FROM sage.config
			 WHERE key = $1 AND database_id = $2 FOR UPDATE`,
			configGenerationKey, databaseID,
		).Scan(&value)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read locked config generation: %w", err)
	}
	var generation uint64
	if _, err := fmt.Sscan(value, &generation); err != nil || generation == 0 {
		return 0, fmt.Errorf("invalid durable config generation %q", value)
	}
	return generation, nil
}

func persistConfigGeneration(
	ctx context.Context, tx pgx.Tx, databaseID, userID int,
	generation uint64,
) error {
	var dbID *int
	if databaseID > 0 {
		dbID = &databaseID
	}
	var changedBy *int
	if userID > 0 {
		changedBy = &userID
	}
	_, err := tx.Exec(ctx,
		`/* pg_sage */ INSERT INTO sage.config
		 (key, value, database_id, updated_at, updated_by_user_id)
		 VALUES ($1, $2, $3, now(), $4)
		 ON CONFLICT (key, COALESCE(database_id, 0))
		 DO UPDATE SET value = EXCLUDED.value, updated_at = now(),
		 updated_by_user_id = EXCLUDED.updated_by_user_id`,
		configGenerationKey, fmt.Sprint(generation), dbID, changedBy,
	)
	if err != nil {
		return fmt.Errorf("persist config generation: %w", err)
	}
	return nil
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

func persistConfigOverride(
	ctx context.Context, tx pgx.Tx, write ConfigOverrideWrite,
	databaseID int, userID int,
) error {
	oldValue, err := getOldValue(ctx, tx, write.Key, databaseID)
	if err != nil {
		return fmt.Errorf("reading old value: %w", err)
	}
	if err := upsertOverride(
		ctx, tx, write.Key, write.Value, databaseID, userID,
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
