package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// maxDiscoveryExtendHours bounds one TTL extension (the ttl_hours bound).
const maxDiscoveryExtendHours = 720

// List returns every discovered row, newest first.
func (s *DiscoveryStore) List(ctx context.Context) ([]DiscoveredRow, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT `+discoveredColumns+`
		 FROM sage.discovered_databases d ORDER BY d.source, d.id LIMIT 1000`)
	if err != nil {
		return nil, fmt.Errorf("listing discovered databases: %w", err)
	}
	return scanDiscovered(rows)
}

// Sources returns each discovery source's last run.
func (s *DiscoveryStore) Sources(ctx context.Context) ([]DiscoverySourceStatus, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT source, last_run_at,
		    last_success_at, last_error, last_count
		 FROM sage.discovery_sources ORDER BY source`)
	if err != nil {
		return nil, fmt.Errorf("listing discovery sources: %w", err)
	}
	defer rows.Close()
	var out []DiscoverySourceStatus
	for rows.Next() {
		var r DiscoverySourceStatus
		if err := rows.Scan(&r.Source, &r.LastRunAt, &r.LastSuccessAt, &r.LastError,
			&r.LastCount); err != nil {
			return nil, fmt.Errorf("scanning discovery source: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Adopt hands an enrolled member to its owner: the TTL is cleared and the
// trust guard lets the normal trust paths change it from now on.
func (s *DiscoveryStore) Adopt(ctx context.Context, rowID int64, userID int) error {
	return s.act(ctx, rowID, userID, DiscoveryEnrolled, DiscoveryAdopted,
		func(ctx context.Context, tx pgx.Tx, databaseID int) error {
			_, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.databases SET
				    adopted_at = now(), adopted_by = $2, expires_at = NULL,
				    updated_at = now()
				 WHERE id = $1`, databaseID, nullUserID(userID))
			return err
		})
}

// Ignore stops a candidate from being enrolled or reported again.
func (s *DiscoveryStore) Ignore(ctx context.Context, rowID int64, userID int) error {
	return s.act(ctx, rowID, userID, DiscoveryCandidate, DiscoveryIgnored, nil)
}

// ExtendTTL pushes an enrolled, unadopted member's expiry hours further
// (from now when it is already due) and returns the new expiry.
func (s *DiscoveryStore) ExtendTTL(ctx context.Context, rowID int64, hours, userID int) (
	time.Time, error) {
	if hours < 1 || hours > maxDiscoveryExtendHours {
		return time.Time{}, fmt.Errorf("%w: hours must be 1-%d, got %d", ErrValidation,
			maxDiscoveryExtendHours, hours)
	}
	var expires time.Time
	err := s.act(ctx, rowID, userID, DiscoveryEnrolled, DiscoveryEnrolled,
		func(ctx context.Context, tx pgx.Tx, databaseID int) error {
			return tx.QueryRow(ctx, `/* pg_sage */ UPDATE sage.databases SET
				    expires_at = GREATEST(expires_at, now()) + make_interval(hours => $2),
				    updated_at = now()
				 WHERE id = $1 AND adopted_at IS NULL RETURNING expires_at`,
				databaseID, hours).Scan(&expires)
		})
	return expires, err
}

// act runs one audited owner action on row rowID, which must be in status
// from; member runs against the row's member when it has one.
func (s *DiscoveryStore) act(ctx context.Context, rowID int64, userID int, from, to string,
	member func(context.Context, pgx.Tx, int) error) error {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(qctx)
	if err != nil {
		return fmt.Errorf("begin discovery action: %w", err)
	}
	defer func() { _ = tx.Rollback(qctx) }()
	row, err := lockDiscovered(qctx, tx, rowID)
	if err != nil {
		return err
	}
	if row.Status != from || (member != nil && row.DatabaseID == nil) {
		return fmt.Errorf("%w: discovered database %d is %s; this needs %s",
			ErrValidation, rowID, row.Status, from)
	}
	if member != nil {
		if err := member(qctx, tx, *row.DatabaseID); err != nil {
			return fmt.Errorf("updating member of discovered database %d: %w", rowID,
				DiscoveredTrustError(err))
		}
	}
	if _, err := tx.Exec(qctx, `/* pg_sage */ UPDATE sage.discovered_databases
		 SET status = $2, status_changed_at = now() WHERE id = $1`, rowID, to); err != nil {
		return fmt.Errorf("updating discovered database %d: %w", rowID, err)
	}
	key := "fleet.discovery." + to
	if from == to {
		key = "fleet.discovery.extend"
	}
	if err := auditDiscovered(qctx, tx, key, from, row, userID); err != nil {
		return err
	}
	if err := tx.Commit(qctx); err != nil {
		return fmt.Errorf("commit discovery action: %w", err)
	}
	return nil
}

func lockDiscovered(ctx context.Context, tx pgx.Tx, rowID int64) (DiscoveredRow, error) {
	var r DiscoveredRow
	err := tx.QueryRow(ctx, `/* pg_sage */ SELECT id, source, external_id, status,
		    database_id FROM sage.discovered_databases WHERE id = $1 FOR UPDATE`, rowID,
	).Scan(&r.ID, &r.Source, &r.ExternalID, &r.Status, &r.DatabaseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("%w: discovered database %d", ErrNotFound, rowID)
	}
	if err != nil {
		return r, fmt.Errorf("locking discovered database %d: %w", rowID, err)
	}
	return r, nil
}

// transition moves row rowID from status from ("" = any) to to, with the
// member id when given, in one audited-or-not statement.
func (s *DiscoveryStore) transition(ctx context.Context, rowID int64, from, to string,
	databaseID *int, reason string) (DiscoveredRow, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var r DiscoveredRow
	err := s.pool.QueryRow(qctx, `/* pg_sage */ UPDATE sage.discovered_databases SET
		    status = $3, status_changed_at = now(),
		    database_id = COALESCE($4, database_id), last_error = $5
		 WHERE id = $1 AND ($2 = '' OR status = $2)
		 RETURNING id, source, external_id, status, database_id`,
		rowID, from, to, databaseID, reason,
	).Scan(&r.ID, &r.Source, &r.ExternalID, &r.Status, &r.DatabaseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("%w: discovered database %d is not %s", ErrValidation,
			rowID, from)
	}
	if err != nil {
		return r, fmt.Errorf("moving discovered database %d to %s: %w", rowID, to, err)
	}
	return r, nil
}

// audit records a sidecar-made discovery event in sage.config_audit.
func (s *DiscoveryStore) audit(ctx context.Context, key string, row DiscoveredRow,
	userID int) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(qctx)
	if err != nil {
		return fmt.Errorf("begin discovery audit: %w", err)
	}
	defer func() { _ = tx.Rollback(qctx) }()
	if err := auditDiscovered(qctx, tx, key, "", row, userID); err != nil {
		return err
	}
	if err := tx.Commit(qctx); err != nil {
		return fmt.Errorf("commit discovery audit: %w", err)
	}
	return nil
}

func auditDiscovered(ctx context.Context, tx pgx.Tx, key, oldStatus string,
	row DiscoveredRow, userID int) error {
	databaseID := 0
	if row.DatabaseID != nil {
		databaseID = *row.DatabaseID
	}
	value := fmt.Sprintf("%s/%s", row.Source, row.ExternalID)
	if err := insertAudit(ctx, tx, key, oldStatus, value, databaseID, userID); err != nil {
		return fmt.Errorf("auditing %s for discovered database %d: %w", key, row.ID, err)
	}
	return nil
}

func nullUserID(userID int) *int {
	if userID < 1 {
		return nil
	}
	return &userID
}
