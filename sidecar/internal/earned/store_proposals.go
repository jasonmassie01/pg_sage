package earned

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Proposal statuses.
const (
	StatusPending    = "pending"
	StatusApproved   = "approved"
	StatusRejected   = "rejected"
	StatusExpired    = "expired"
	StatusSuperseded = "superseded"
)

// Proposal is pg_sage's promotion proposal for one pair, one level up.
type Proposal struct {
	ID             string          `json:"id"`
	Family         Family          `json:"family"`
	Class          ActionClass     `json:"class"`
	From           Level           `json:"from"`
	To             Level           `json:"to"`
	Evidence       json.RawMessage `json:"evidence"`
	EvidenceSHA256 string          `json:"evidence_sha256"`
	Status         string          `json:"status"`
	ProposedAt     time.Time       `json:"proposed_at"`
	ExpiresAt      time.Time       `json:"expires_at"`
	DecidedBy      string          `json:"decided_by,omitempty"`
	DecidedAt      *time.Time      `json:"decided_at,omitempty"`
	Note           string          `json:"note,omitempty"`
}

const proposalColumns = `id::text, family, action_class, from_level, to_level, evidence,
	encode(evidence_sha256, 'hex'), status, proposed_at, expires_at,
	COALESCE(decided_by, ''), decided_at, COALESCE(decision_note, '')`

func scanProposal(row pgx.Row) (Proposal, error) {
	var p Proposal
	var family, class string
	var from, to int16
	err := row.Scan(&p.ID, &family, &class, &from, &to, &p.Evidence, &p.EvidenceSHA256,
		&p.Status, &p.ProposedAt, &p.ExpiresAt, &p.DecidedBy, &p.DecidedAt, &p.Note)
	p.Family, p.Class, p.From, p.To = Family(family), ActionClass(class), Level(from),
		Level(to)
	return p, err
}

// insertProposal stores a pending proposal; false when one is already
// pending for the pair.
func (s *PostgresStore) insertProposal(ctx context.Context, q querier, p Proposal) (bool,
	error) {
	sum := sha256.Sum256(p.Evidence)
	_, err := q.Exec(ctx, `INSERT INTO sage.sre_autonomy_proposals
		(deployment_id, id, family, action_class, from_level, to_level, evidence,
		 evidence_sha256, status, proposed_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', $9, $10)`,
		s.deployment, p.ID, string(p.Family), string(p.Class), int16(p.From), int16(p.To),
		p.Evidence, sum[:], p.ProposedAt, p.ExpiresAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return false, nil
	}
	return err == nil, storeErr("insert autonomy proposal", err)
}

// Proposal reads one proposal.
func (s *PostgresStore) Proposal(ctx context.Context, id string) (Proposal, error) {
	return s.readProposal(ctx, s.pool, id, false)
}

func (s *PostgresStore) readProposal(ctx context.Context, q querier, id string,
	forUpdate bool) (Proposal, error) {
	if !uuidPattern.MatchString(id) {
		return Proposal{}, fmt.Errorf("%w: proposal id %q", ErrInvalidRequest, id)
	}
	query := `SELECT ` + proposalColumns + ` FROM sage.sre_autonomy_proposals
		WHERE deployment_id = $1 AND id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	p, err := scanProposal(q.QueryRow(ctx, query, s.deployment, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return p, storeErr("read autonomy proposal", err)
}

// listProposals lists proposals of a status, newest first.
func (s *PostgresStore) listProposals(ctx context.Context, status string, limit int) (
	[]Proposal, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+proposalColumns+`
		FROM sage.sre_autonomy_proposals WHERE deployment_id = $1 AND status = $2
		ORDER BY proposed_at DESC, id LIMIT $3`, s.deployment, status, limit)
	if err != nil {
		return nil, storeErr("list autonomy proposals", err)
	}
	defer rows.Close()
	out := []Proposal{}
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, storeErr("scan autonomy proposal", err)
		}
		out = append(out, p)
	}
	return out, storeErr("list autonomy proposals", rows.Err())
}

// decideProposal moves a pending proposal to status.
func (s *PostgresStore) decideProposal(ctx context.Context, q querier, id, status,
	actor, note string, at time.Time) error {
	tag, err := q.Exec(ctx, `UPDATE sage.sre_autonomy_proposals
		SET status = $3, decided_by = $4, decision_note = NULLIF($5, ''), decided_at = $6
		WHERE deployment_id = $1 AND id = $2 AND status = 'pending'`,
		s.deployment, id, status, actor, note, at)
	if err != nil {
		return storeErr("decide autonomy proposal", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: %s", ErrNotPending, id)
	}
	return nil
}

// closePending supersedes (or expires) the pending proposals of a pair,
// or of every class of the family when class is AllClasses, returning
// the closed proposals.
func (s *PostgresStore) closePending(ctx context.Context, q querier, f Family,
	c ActionClass, status, actor, note string, at time.Time) ([]Proposal, error) {
	rows, err := q.Query(ctx, `UPDATE sage.sre_autonomy_proposals
		SET status = $4, decided_by = $5, decision_note = $6, decided_at = $7
		WHERE deployment_id = $1 AND family = $2 AND status = 'pending'
		  AND ($3 = '*' OR action_class = $3)
		RETURNING `+proposalColumns, s.deployment, string(f), string(c), status, actor,
		note, at)
	if err != nil {
		return nil, storeErr("close autonomy proposals", err)
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, storeErr("scan closed proposal", err)
		}
		out = append(out, p)
	}
	return out, storeErr("close autonomy proposals", rows.Err())
}

// expireDue expires pending proposals past their expiry.
func (s *PostgresStore) expireDue(ctx context.Context, at time.Time) ([]Proposal, error) {
	rows, err := s.pool.Query(ctx, `UPDATE sage.sre_autonomy_proposals
		SET status = 'expired', decided_by = $2, decision_note = 'expired', decided_at = $3
		WHERE deployment_id = $1 AND status = 'pending' AND expires_at <= $3
		RETURNING `+proposalColumns, s.deployment, ActorPgSage, at)
	if err != nil {
		return nil, storeErr("expire autonomy proposals", err)
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, storeErr("scan expired proposal", err)
		}
		out = append(out, p)
	}
	return out, storeErr("expire autonomy proposals", rows.Err())
}
