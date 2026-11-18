package gitops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
)

// sage.approval_prs on the control database (cli-02b). Every state change
// is one conditional UPDATE from a known state and its audit entry, in one
// transaction, so each transition happens and is audited once even when
// two replicas poll at the same moment.

// ApprovalPRRecord is one approval PR row.
type ApprovalPRRecord struct {
	ID            int64      `json:"id"`
	Database      string     `json:"database"`
	QueueID       int        `json:"queue_id"`
	Repo          string     `json:"repo"`
	Path          string     `json:"path"`
	PRNumber      int        `json:"pr_number,omitempty"`
	PRURL         string     `json:"pr_url,omitempty"`
	State         string     `json:"state"`
	CardHash      string     `json:"card_hash"`
	FileHash      string     `json:"-"`
	Detail        string     `json:"detail,omitempty"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	ExpiresAt     time.Time  `json:"expires_at"`
	OpenedAt      *time.Time `json:"opened_at,omitempty"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	DecidedBy     int        `json:"decided_by,omitempty"`
}

const approvalPRColumns = `id, database, queue_id, repo, path, COALESCE(pr_number, 0),
	pr_url, state, card_hash, file_hash, detail, attempts, next_attempt_at, expires_at,
	opened_at, decided_at, COALESCE(decided_by, 0)`

func scanApprovalPR(row pgx.Row) (ApprovalPRRecord, error) {
	var r ApprovalPRRecord
	err := row.Scan(&r.ID, &r.Database, &r.QueueID, &r.Repo, &r.Path, &r.PRNumber,
		&r.PRURL, &r.State, &r.CardHash, &r.FileHash, &r.Detail, &r.Attempts,
		&r.NextAttemptAt, &r.ExpiresAt, &r.OpenedAt, &r.DecidedAt, &r.DecidedBy)
	return r, err
}

// LookupApprovalPR returns the approval PR of one change, or nil.
func LookupApprovalPR(ctx context.Context, pool *pgxpool.Pool, database string,
	queueID int) (*ApprovalPRRecord, error) {
	if pool == nil {
		return nil, errors.New("gitops: no control database for approval PRs")
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := scanApprovalPR(pool.QueryRow(qctx, `/* pg_sage */ SELECT `+
		approvalPRColumns+` FROM sage.approval_prs WHERE database = $1 AND queue_id = $2`,
		database, queueID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the approval PR of %s/%d: %w", database, queueID, err)
	}
	return &r, nil
}

// listApprovalPRs returns the rows in state, oldest first.
func listApprovalPRs(ctx context.Context, pool *pgxpool.Pool, state string,
) ([]ApprovalPRRecord, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := pool.Query(qctx, `/* pg_sage */ SELECT `+approvalPRColumns+`
		FROM sage.approval_prs WHERE state = $1 ORDER BY id LIMIT 500`, state)
	if err != nil {
		return nil, fmt.Errorf("listing %s approval PRs: %w", state, err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ApprovalPRRecord, error) {
		return scanApprovalPR(r)
	})
}

// claimApprovalPR inserts the row of a new change in state opening; ok is
// false when the change already has one (never a second PR).
func claimApprovalPR(ctx context.Context, pool *pgxpool.Pool, rec ApprovalPRRecord,
) (ApprovalPRRecord, bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := scanApprovalPR(pool.QueryRow(qctx, `/* pg_sage */ INSERT INTO
		sage.approval_prs (database, queue_id, repo, path, card_hash, expires_at, state)
		VALUES ($1, $2, $3, $4, $5, $6, 'opening')
		ON CONFLICT (database, queue_id) DO NOTHING RETURNING `+approvalPRColumns,
		rec.Database, rec.QueueID, rec.Repo, rec.Path, rec.CardHash, rec.ExpiresAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return ApprovalPRRecord{}, false, nil
	}
	if err != nil {
		return ApprovalPRRecord{}, false, fmt.Errorf("claiming the approval PR of %s/%d: %w",
			rec.Database, rec.QueueID, err)
	}
	return r, true, nil
}

// transition is one state change of a row.
type transition struct {
	ID       int64
	From, To string
	Detail   string
	PRNumber int
	PRURL    string
	FileHash string
	CardHash string
	// NextAttempt and Failed record an open failure (attempts + 1).
	NextAttempt *time.Time
	Failed      bool
	Opened      bool
	// DecidedBy is the approver of a merge (0: none); Decided stamps it.
	DecidedBy int
	Decided   bool
}

const transitionSQL = `/* pg_sage */ UPDATE sage.approval_prs SET state = $3,
	detail = $4,
	pr_number = COALESCE(NULLIF($5, 0), pr_number),
	pr_url = COALESCE(NULLIF($6, ''), pr_url),
	file_hash = COALESCE(NULLIF($7, ''), file_hash),
	next_attempt_at = $8,
	attempts = attempts + CASE WHEN $9 THEN 1 ELSE 0 END,
	opened_at = CASE WHEN $10 THEN now() ELSE opened_at END,
	decided_at = CASE WHEN $11 THEN now() ELSE decided_at END,
	decided_by = COALESCE(NULLIF($12, 0), decided_by),
	card_hash = COALESCE(NULLIF($13, ''), card_hash)
	WHERE id = $1 AND state = $2
	RETURNING database, queue_id, repo, COALESCE(pr_number, 0)`

// apply makes the transition and audits it in one transaction; ok is
// false when the row had already left From (another poll moved it).
func (tr transition) apply(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ok := false
	err := pgx.BeginFunc(qctx, pool, func(tx pgx.Tx) error {
		var db, repo string
		var queueID, number int
		err := tx.QueryRow(qctx, transitionSQL, tr.ID, tr.From, tr.To, tr.Detail,
			tr.PRNumber, tr.PRURL, tr.FileHash, tr.NextAttempt, tr.Failed, tr.Opened,
			tr.Decided, tr.DecidedBy, tr.CardHash).Scan(&db, &queueID, &repo, &number)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		ok = true
		return auth.RecordAuthAudit(qctx, tx, auth.AuthAuditEvent{Event: AuditApprovalPR,
			ActorUserID: tr.DecidedBy, Detail: map[string]any{"database": db,
				"queue_id": queueID, "repo": repo, "pr_number": number, "from": tr.From,
				"to": tr.To, "detail": tr.Detail}})
	})
	if err != nil {
		return false, fmt.Errorf("approval PR %d %s -> %s: %w", tr.ID, tr.From, tr.To, err)
	}
	return ok, nil
}
