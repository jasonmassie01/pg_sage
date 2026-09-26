package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SetDatabaseTrustPolicy durably sets one managed database's trust level
// (sage.databases.trust_level), audits the change as a system write, and
// removes legacy per-database trust overrides that could shadow it after
// restart. It returns the previous level so callers can roll back. Used
// when a global trust downgrade caps meta-db databases (G5-B14).
func (s *ConfigStore) SetDatabaseTrustPolicy(
	ctx context.Context, databaseID int, level string,
) (string, error) {
	if err := ValidateConfigOverride("trust.level", level); err != nil {
		return "", err
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(qctx)
	if err != nil {
		return "", fmt.Errorf("begin database trust policy: %w", err)
	}
	defer func() { _ = tx.Rollback(qctx) }()
	var previous string
	err = tx.QueryRow(qctx,
		`/* pg_sage */ SELECT trust_level FROM sage.databases
		 WHERE id = $1 FOR UPDATE`, databaseID,
	).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: id %d", ErrConfigDatabaseNotFound, databaseID)
	}
	if err != nil {
		return "", fmt.Errorf("lock database trust policy: %w", err)
	}
	if _, err := tx.Exec(qctx,
		`/* pg_sage */ UPDATE sage.databases SET trust_level = $1 WHERE id = $2`,
		level, databaseID,
	); err != nil {
		return "", fmt.Errorf("update database trust policy: %w", err)
	}
	if err := insertAudit(qctx, tx, "trust.level",
		previous, level, databaseID, 0); err != nil {
		return "", fmt.Errorf("audit database trust policy: %w", err)
	}
	if err := deleteConfigOverride(qctx, tx, "trust.level", databaseID); err != nil {
		return "", err
	}
	if err := tx.Commit(qctx); err != nil {
		return "", fmt.Errorf("commit database trust policy: %w", err)
	}
	return previous, nil
}
