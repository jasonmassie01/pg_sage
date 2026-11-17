package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// expiryActor names the sweep in the audit trail.
const expiryActor = "system-exception-expiry"

// ExceptionRequest gives a database its own document: a copy of its set's
// current document, which then changes only through Propose and Ratify on
// the database's scope. ExpiresAt nil holds until cleared.
type ExceptionRequest struct {
	DatabaseID int64
	Actor      string
	Reason     string
	ExpiresAt  *time.Time
}

func (s *Store) validateException(req ExceptionRequest) error {
	switch {
	case req.DatabaseID <= 0:
		return invalidSet("an exception needs a positive database_id")
	case strings.TrimSpace(req.Actor) == "":
		return invalidSet("an exception needs the actor setting it")
	case strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 2000:
		return invalidSet("an exception needs a reason of 1-2000 characters")
	case req.ExpiresAt != nil && !req.ExpiresAt.After(s.clock()):
		return invalidSet("expires_at %s is not in the future", req.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// SetException records an exception for a database that follows a set.
// A database with its own document in force is ErrExceptionExists; one
// whose exception expired is swept first.
func (s *Store) SetException(ctx context.Context, req ExceptionRequest) (Exception, error) {
	if s == nil || s.pool == nil {
		return Exception{}, ErrUnavailable
	}
	if err := s.validateException(req); err != nil {
		return Exception{}, err
	}
	var out Exception
	err := s.inLockedTx(ctx, req.DatabaseID, func(tx pgx.Tx) error {
		snap, err := loadSnapshot(ctx, tx, &req.DatabaseID)
		if err != nil {
			return err
		}
		res, err := snap.resolve(req.DatabaseID, s.clock())
		if err != nil {
			return err
		}
		if res.Exception != nil {
			return fmt.Errorf("%w: database %d (set by %s)", ErrExceptionExists,
				req.DatabaseID, res.Exception.SetBy)
		}
		if res.ExceptionExpired {
			if err := expireException(ctx, tx, req.DatabaseID, res.Set); err != nil {
				return err
			}
		}
		out, err = insertException(ctx, tx, req, res)
		return err
	})
	return out, err
}

func insertException(
	ctx context.Context, tx pgx.Tx, req ExceptionRequest, res Resolution,
) (Exception, error) {
	var id, version int64
	err := tx.QueryRow(ctx, `
INSERT INTO sage.policy (database_id, version, scope, profile, doc, status, proposed_by,
    ratified_by, ratified_at, activated_at)
SELECT $1, COALESCE(max(version), 0) + 1, 'database', $2, $3, 'active', $4, $4, now(), now()
FROM sage.policy WHERE database_id = $1 AND version > 0
RETURNING id, version`, req.DatabaseID, res.Policy.Profile, res.Policy.Document,
		req.Actor).Scan(&id, &version)
	if err != nil {
		return Exception{}, fmt.Errorf("copy set %s's document for database %d: %w",
			res.Set, req.DatabaseID, err)
	}
	out := Exception{DatabaseID: req.DatabaseID, SetName: res.Set, Diff: []FieldDiff{},
		SetBy: req.Actor, Reason: req.Reason, ExpiresAt: req.ExpiresAt,
		PolicyID: id, PolicyVersion: version}
	err = tx.QueryRow(ctx, `
INSERT INTO sage.policy_exceptions (database_id, set_name, diff, set_by, reason, expires_at)
VALUES ($1, $2, '[]', $3, $4, $5)
ON CONFLICT (database_id) DO UPDATE SET set_name = EXCLUDED.set_name, diff = '[]',
    set_by = EXCLUDED.set_by, reason = EXCLUDED.reason, set_at = now(),
    expires_at = EXCLUDED.expires_at
RETURNING set_at`, req.DatabaseID, res.Set, req.Actor, req.Reason, req.ExpiresAt).
		Scan(&out.SetAt)
	if err != nil {
		return Exception{}, fmt.Errorf("record exception for database %d: %w",
			req.DatabaseID, err)
	}
	detail, _ := json.Marshal(map[string]any{"reason": req.Reason, "expires_at": req.ExpiresAt,
		"policy_version": version})
	return out, recordSetEvent(ctx, tx, "exception_set", res.Set, &req.DatabaseID,
		req.Actor, detail)
}

// ExpireExceptions reverts every database whose exception expired (at or
// before now) to its set: its own document is superseded, the exception
// removed and an exception_expired audit row written. It returns how many.
func (s *Store) ExpireExceptions(ctx context.Context) (int, error) {
	if s == nil || s.pool == nil {
		return 0, ErrUnavailable
	}
	count := 0
	err := s.inLockedTx(ctx, setsLockKey, func(tx pgx.Tx) error {
		expired, err := expiredExceptions(ctx, tx, s.clock())
		if err != nil {
			return err
		}
		for id, set := range expired {
			if err := expireException(ctx, tx, id, set); err != nil {
				return err
			}
		}
		count = len(expired)
		return nil
	})
	return count, err
}

func expiredExceptions(ctx context.Context, tx pgx.Tx, now time.Time) (map[int64]string, error) {
	rows, err := tx.Query(ctx, `SELECT database_id, set_name FROM sage.policy_exceptions
WHERE expires_at <= $1 FOR UPDATE`, now)
	if err != nil {
		return nil, fmt.Errorf("read expired policy exceptions: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var set string
		if err := rows.Scan(&id, &set); err != nil {
			return nil, fmt.Errorf("scan expired policy exception: %w", err)
		}
		out[id] = set
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired policy exceptions: %w", err)
	}
	return out, nil
}

func expireException(ctx context.Context, tx pgx.Tx, databaseID int64, set string) error {
	if _, err := tx.Exec(ctx, `UPDATE sage.policy SET status = 'superseded'
WHERE database_id = $1 AND set_name IS NULL AND status = 'active'`, databaseID); err != nil {
		return fmt.Errorf("supersede database %d's expired exception: %w", databaseID, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sage.policy_exceptions WHERE database_id = $1`,
		databaseID); err != nil {
		return fmt.Errorf("remove database %d's expired exception: %w", databaseID, err)
	}
	return recordSetEvent(ctx, tx, "exception_expired", set, &databaseID, expiryActor, nil)
}
