package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

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
		if err := s.persistConfigOverride(
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
// execution mode row under the database-scoped generation predicate. In one
// transaction it takes the generation lock, compares the generation, writes
// the database policy row, then the overrides, then the next generation.
func (s *ConfigStore) SetDatabaseOverridesCAS(
	ctx context.Context, writes []ConfigOverrideWrite,
	databaseID int, userID int, expected uint64,
	executionMode *string,
) (uint64, error) {
	if err := validateDatabaseCASArgs(writes, databaseID, expected); err != nil {
		return 0, err
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
	if err := updateDatabasePolicy(qctx, tx, databaseID, userID,
		executionMode, trustLevelWrite(writes)); err != nil {
		return 0, err
	}
	if err := s.writeDatabaseOverrides(qctx, tx, writes, databaseID, userID); err != nil {
		return 0, err
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

// validateDatabaseCASArgs refuses a database revision before any
// transaction opens: the database id, then each write's key and value,
// then the expected generation.
func validateDatabaseCASArgs(
	writes []ConfigOverrideWrite, databaseID int, expected uint64,
) error {
	if databaseID < 1 {
		return fmt.Errorf("database id must be positive")
	}
	for _, write := range writes {
		if err := validateConfigKey(write.Key); err != nil {
			return err
		}
		if err := validateConfigValue(write.Key, write.Value); err != nil {
			return err
		}
	}
	if expected == 0 {
		return fmt.Errorf("expected generation must be positive")
	}
	return nil
}

// trustLevelWrite is the value of the last trust.level write, or nil: trust
// is stored on the database policy row, not as an override.
func trustLevelWrite(writes []ConfigOverrideWrite) *string {
	var trustLevel *string
	for i := range writes {
		if writes[i].Key == "trust.level" {
			trustLevel = &writes[i].Value
		}
	}
	return trustLevel
}

// updateDatabasePolicy locks and updates the database policy row when the
// revision sets the execution mode or trust level, audits each change and
// deletes the legacy trust.level override.
func updateDatabasePolicy(
	ctx context.Context, tx pgx.Tx, databaseID, userID int,
	executionMode, trustLevel *string,
) error {
	if executionMode == nil && trustLevel == nil {
		return nil
	}
	var oldTrust string
	var oldExecutionMode string
	err := tx.QueryRow(ctx,
		`/* pg_sage */ SELECT trust_level, execution_mode FROM sage.databases
			 WHERE id = $1 FOR UPDATE`, databaseID,
	).Scan(&oldTrust, &oldExecutionMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: id %d", ErrConfigDatabaseNotFound, databaseID)
	}
	if err != nil {
		return fmt.Errorf("lock database policy: %w", err)
	}
	_, err = tx.Exec(ctx,
		`/* pg_sage */ UPDATE sage.databases SET
			 execution_mode = COALESCE($1, execution_mode),
			 trust_level = COALESCE($2, trust_level)
			 WHERE id = $3`, executionMode, trustLevel, databaseID,
	)
	if err != nil {
		return fmt.Errorf("update database policy: %w", DiscoveredTrustError(err))
	}
	if executionMode != nil {
		if err := insertAudit(ctx, tx, "execution_mode",
			oldExecutionMode, *executionMode, databaseID, userID); err != nil {
			return fmt.Errorf("audit database execution mode: %w", err)
		}
	}
	if trustLevel == nil {
		return nil
	}
	if err := insertAudit(ctx, tx, "trust.level",
		oldTrust, *trustLevel, databaseID, userID); err != nil {
		return fmt.Errorf("audit database trust policy: %w", err)
	}
	// Remove legacy higher-priority rows so the canonical database
	// policy cannot be shadowed after restart or reconnect.
	return deleteConfigOverride(ctx, tx, "trust.level", databaseID)
}

// writeDatabaseOverrides persists every write except trust.level, which
// updateDatabasePolicy stores on the database row.
func (s *ConfigStore) writeDatabaseOverrides(
	ctx context.Context, tx pgx.Tx, writes []ConfigOverrideWrite,
	databaseID int, userID int,
) error {
	for _, write := range writes {
		if write.Key == "trust.level" {
			continue
		}
		if err := s.persistConfigOverride(
			ctx, tx, write, databaseID, userID,
		); err != nil {
			return err
		}
	}
	return nil
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
