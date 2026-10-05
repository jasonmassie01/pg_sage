package managedparam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StatusPending    = "pending"
	StatusApproved   = "approved"
	StatusRejected   = "rejected"
	StatusApplied    = "applied"
	StatusSuperseded = "superseded"
)

// RejectionCooldown is how long a rejected change is not proposed again.
const RejectionCooldown = 7 * 24 * time.Hour

var (
	ErrNotFound   = errors.New("managed change proposal not found")
	ErrNotPending = errors.New("managed change proposal is no longer pending")
)

var validStatus = map[string]bool{StatusPending: true, StatusApproved: true,
	StatusRejected: true, StatusApplied: true, StatusSuperseded: true}

// Record is one stored proposal with its lifecycle.
type Record struct {
	ID           int64      `json:"id"`
	Status       string     `json:"status"`
	Proposal     Proposal   `json:"proposal"`
	FindingID    *int64     `json:"finding_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DecidedBy    *int       `json:"decided_by,omitempty"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	DecisionNote string     `json:"decision_note,omitempty"`
	AppliedAt    *time.Time `json:"applied_at,omitempty"`
}

// Store persists proposals in sage.managed_change_proposals.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a store over the monitored database's pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const recordColumns = `id, status, proposal, finding_id, created_at, updated_at,
	decided_by, decided_at, decision_note, applied_at`

func scanRecord(row pgx.Row) (Record, error) {
	var r Record
	var raw []byte
	if err := row.Scan(&r.ID, &r.Status, &raw, &r.FindingID, &r.CreatedAt, &r.UpdatedAt,
		&r.DecidedBy, &r.DecidedAt, &r.DecisionNote, &r.AppliedAt); err != nil {
		return Record{}, err
	}
	if err := json.Unmarshal(raw, &r.Proposal); err != nil {
		return Record{}, fmt.Errorf("decode managed change %d: %w", r.ID, err)
	}
	return r, nil
}

// Upsert records p. An open proposal with the same fingerprint is
// refreshed (created=false); a recently rejected one is returned
// unchanged; otherwise open proposals for the same parameter and target
// are superseded and p is inserted as pending.
func (s *Store) Upsert(ctx context.Context, p Proposal, findingID int64) (Record, bool, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return Record{}, false, fmt.Errorf("encode managed change: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Record{}, false, fmt.Errorf("begin managed change upsert: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rec, done, err := s.existing(ctx, tx, p, raw)
	if err != nil || done {
		if err == nil {
			err = tx.Commit(ctx)
		}
		return rec, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE sage.managed_change_proposals
		SET status = 'superseded', updated_at = now()
		WHERE provider = $1 AND target = $2 AND parameter = $3
		  AND status IN ('pending', 'approved')`, p.Provider, p.Target, p.Parameter); err != nil {
		return Record{}, false, fmt.Errorf("supersede older managed changes: %w", err)
	}
	var finding any
	if findingID > 0 {
		finding = findingID
	}
	rec, err = scanRecord(tx.QueryRow(ctx, `INSERT INTO sage.managed_change_proposals
		(fingerprint, provider, mechanism, target, parameter, value, reboot_required,
		 proposal, finding_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+recordColumns,
		p.Fingerprint, p.Provider, p.Mechanism, p.Target, p.Parameter, p.Value,
		p.RebootRequired, raw, finding))
	if err != nil {
		return Record{}, false, fmt.Errorf("insert managed change: %w", err)
	}
	return rec, true, tx.Commit(ctx)
}

// existing refreshes an open proposal with p's fingerprint, or finds a
// rejection inside the cooldown; done reports that one was found.
func (s *Store) existing(ctx context.Context, tx pgx.Tx, p Proposal,
	raw []byte) (Record, bool, error) {
	rec, err := scanRecord(tx.QueryRow(ctx, `UPDATE sage.managed_change_proposals
		SET proposal = $2, updated_at = now()
		WHERE fingerprint = $1 AND status IN ('pending', 'approved')
		RETURNING `+recordColumns, p.Fingerprint, raw))
	if err == nil {
		return rec, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Record{}, false, fmt.Errorf("refresh managed change: %w", err)
	}
	rec, err = scanRecord(tx.QueryRow(ctx, `SELECT `+recordColumns+`
		FROM sage.managed_change_proposals
		WHERE fingerprint = $1 AND status = 'rejected'
		  AND decided_at > now() - make_interval(secs => $2)
		ORDER BY decided_at DESC LIMIT 1`, p.Fingerprint, RejectionCooldown.Seconds()))
	if err == nil {
		return rec, true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, false, nil
	}
	return Record{}, false, fmt.Errorf("read rejected managed change: %w", err)
}

// Get returns one proposal.
func (s *Store) Get(ctx context.Context, id int64) (Record, error) {
	rec, err := scanRecord(s.pool.QueryRow(ctx, `SELECT `+recordColumns+`
		FROM sage.managed_change_proposals WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, fmt.Errorf("%w: %d", ErrNotFound, id)
	}
	return rec, err
}

// List returns the newest proposals with the given statuses (all when
// none), at most limit (1-500).
func (s *Store) List(ctx context.Context, statuses []string, limit int) ([]Record, error) {
	for _, st := range statuses {
		if !validStatus[st] {
			return nil, fmt.Errorf("unknown managed change status %q", st)
		}
	}
	if statuses == nil {
		statuses = []string{}
	}
	limit = max(1, min(limit, 500))
	rows, err := s.pool.Query(ctx, `SELECT `+recordColumns+`
		FROM sage.managed_change_proposals
		WHERE cardinality($1::text[]) = 0 OR status = ANY($1)
		ORDER BY created_at DESC, id DESC LIMIT $2`, statuses, limit)
	if err != nil {
		return nil, fmt.Errorf("list managed changes: %w", err)
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
