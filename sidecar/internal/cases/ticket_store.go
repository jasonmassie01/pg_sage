package cases

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ticketClaimLease is how long a claim without an issue blocks another
// create; a sidecar that died mid-create leaves a claim this old behind.
// It is far above the Jira client's 10 s timeout, so a slow create is
// never taken over.
const ticketClaimLease = 2 * time.Minute

// PgTicketStore is the TicketStore over sage.case_tickets on the control
// database.
type PgTicketStore struct {
	pool *pgxpool.Pool
}

// NewTicketStore returns the store over pool.
func NewTicketStore(pool *pgxpool.Pool) *PgTicketStore {
	return &PgTicketStore{pool: pool}
}

var _ TicketStore = (*PgTicketStore)(nil)

const ticketColumns = `id, database, case_identity_key, system, channel_id,
	coalesce(external_key, ''), coalesce(url, ''), status, status_category, sync_error,
	resolved_comment_at, coalesce(created_by, ''), created_at, last_synced_at`

func scanTicket(row pgx.Row) (Ticket, error) {
	var t Ticket
	err := row.Scan(&t.ID, &t.Database, &t.CaseIdentityKey, &t.System, &t.ChannelID,
		&t.ExternalKey, &t.URL, &t.Status, &t.StatusCategory, &t.SyncError,
		&t.ResolvedCommentAt, &t.CreatedBy, &t.CreatedAt, &t.LastSyncedAt)
	return t, err
}

func (s *PgTicketStore) query(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}

// Claim inserts the claim row, or takes over a claim older than the
// lease that never got an issue, or returns the existing ticket.
func (s *PgTicketStore) Claim(ctx context.Context, c TicketClaim) (Ticket, bool, error) {
	qctx, cancel := s.query(ctx)
	defer cancel()
	t, err := scanTicket(s.pool.QueryRow(qctx, `/* pg_sage */
		INSERT INTO sage.case_tickets (database, case_identity_key, system, channel_id,
		    created_by)
		VALUES ($1, $2, $3, $4, nullif($5, ''))
		ON CONFLICT (database, case_identity_key, system) DO UPDATE
		    SET channel_id = EXCLUDED.channel_id, created_by = EXCLUDED.created_by,
		        created_at = now()
		    WHERE sage.case_tickets.external_key IS NULL
		      AND sage.case_tickets.created_at < now() - make_interval(secs => $6)
		RETURNING `+ticketColumns,
		c.Database, c.CaseIdentityKey, c.System, c.ChannelID, c.CreatedBy,
		ticketClaimLease.Seconds()))
	if err == nil {
		return t, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, false, fmt.Errorf("claiming ticket for %s: %w", c.CaseIdentityKey, err)
	}
	t, err = scanTicket(s.pool.QueryRow(qctx, `/* pg_sage */ SELECT `+ticketColumns+`
		FROM sage.case_tickets
		WHERE database = $1 AND case_identity_key = $2 AND system = $3`,
		c.Database, c.CaseIdentityKey, c.System))
	if err != nil {
		return Ticket{}, false, fmt.Errorf("reading ticket for %s: %w", c.CaseIdentityKey, err)
	}
	return t, false, nil
}

// Complete records the created issue on the claim.
func (s *PgTicketStore) Complete(ctx context.Context, id int64, ref IssueRef) (Ticket, error) {
	qctx, cancel := s.query(ctx)
	defer cancel()
	t, err := scanTicket(s.pool.QueryRow(qctx, `/* pg_sage */
		UPDATE sage.case_tickets SET external_key = $2, url = $3, status = ''
		WHERE id = $1 RETURNING `+ticketColumns, id, ref.Key, ref.URL))
	if err != nil {
		return Ticket{}, fmt.Errorf("completing ticket %d: %w", id, err)
	}
	return t, nil
}

// Release deletes a claim whose issue was never created.
func (s *PgTicketStore) Release(ctx context.Context, id int64) error {
	qctx, cancel := s.query(ctx)
	defer cancel()
	_, err := s.pool.Exec(qctx, `/* pg_sage */ DELETE FROM sage.case_tickets
		WHERE id = $1 AND external_key IS NULL`, id)
	if err != nil {
		return fmt.Errorf("releasing ticket claim %d: %w", id, err)
	}
	return nil
}

// List returns the created tickets of database ("" or "all": every one).
func (s *PgTicketStore) List(ctx context.Context, database string) ([]Ticket, error) {
	qctx, cancel := s.query(ctx)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT `+ticketColumns+`
		FROM sage.case_tickets
		WHERE external_key IS NOT NULL AND ($1 IN ('', 'all') OR database = $1)
		ORDER BY id LIMIT 1000`, database)
	if err != nil {
		return nil, fmt.Errorf("listing case tickets: %w", err)
	}
	return collectTickets(rows)
}

func collectTickets(rows pgx.Rows) ([]Ticket, error) {
	defer rows.Close()
	out := []Ticket{}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning case ticket: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading case tickets: %w", err)
	}
	return out, nil
}
