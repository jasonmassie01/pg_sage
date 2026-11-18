package cases

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// prStoreTimeout bounds one PR-mode statement on the sage schema.
const prStoreTimeout = 5 * time.Second

// PgPRStore is PR mode's PRStore over sage.source_fix and a session
// advisory lock per candidate key, on the database's own pool.
type PgPRStore struct {
	pool *pgxpool.Pool
}

// NewPRStore returns the store over pool.
func NewPRStore(pool *pgxpool.Pool) *PgPRStore { return &PgPRStore{pool: pool} }

var _ PRStore = (*PgPRStore)(nil)

const prLockSQL = `/* pg_sage */ SELECT pg_try_advisory_lock(
    hashtextextended('pg_sage.pr_mode:' || $1, 0))`

const prUnlockSQL = `/* pg_sage */ SELECT pg_advisory_unlock(
    hashtextextended('pg_sage.pr_mode:' || $1, 0))`

// Lock takes key's session advisory lock on a dedicated connection, held
// while the host is asked and the PR opened (no transaction stays open).
// A failed unlock closes the connection, which releases the lock.
func (s *PgPRStore) Lock(ctx context.Context, key string) (func(), bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire a connection: %w", err)
	}
	qctx, cancel := context.WithTimeout(ctx, prStoreTimeout)
	defer cancel()
	var ok bool
	if err := conn.QueryRow(qctx, prLockSQL, key).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("take the advisory lock: %w", err)
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		uctx, cancel := context.WithTimeout(context.Background(), prStoreTimeout)
		defer cancel()
		var released bool
		if err := conn.QueryRow(uctx, prUnlockSQL, key).Scan(&released); err != nil ||
			!released {
			_ = conn.Conn().Close(uctx) // closing the session drops its locks
		}
		conn.Release()
	}, true, nil
}

const prSourceFixSQL = `/* pg_sage */ SELECT origin, COALESCE(repo, ''),
    COALESCE(pr_number, 0), COALESCE(pr_url, ''), stage, COALESCE(pr_state, ''),
    COALESCE(verdict, '')
FROM sage.source_fix WHERE finding_id = $1`

// SourceFix reads the finding's source-fix row; found is false when none.
func (s *PgPRStore) SourceFix(ctx context.Context, findingID int64) (SourceFixPR, bool,
	error) {
	ctx, cancel := context.WithTimeout(ctx, prStoreTimeout)
	defer cancel()
	var r SourceFixPR
	err := s.pool.QueryRow(ctx, prSourceFixSQL, findingID).Scan(&r.Origin, &r.Repo,
		&r.Number, &r.URL, &r.Stage, &r.PRState, &r.Verdict)
	if errors.Is(err, pgx.ErrNoRows) {
		return SourceFixPR{}, false, nil
	}
	if err != nil {
		return SourceFixPR{}, false, err
	}
	return r, true, nil
}

const prSetStateSQL = `/* pg_sage */ UPDATE sage.source_fix
SET pr_state = $2, updated_at = now()
WHERE finding_id = $1 AND origin = 'pg_sage'`

// SetPRState records the last seen state of PR mode's PR on the finding.
func (s *PgPRStore) SetPRState(ctx context.Context, findingID int64, state string) error {
	ctx, cancel := context.WithTimeout(ctx, prStoreTimeout)
	defer cancel()
	tag, err := s.pool.Exec(ctx, prSetStateSQL, findingID, state)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("finding %d has no PR opened by pg_sage", findingID)
	}
	return nil
}
