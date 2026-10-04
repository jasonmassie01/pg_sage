package specialist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Record kinds and outbound states (sage.specialist_requests).
const (
	KindOpen        = "open"
	KindAttach      = "attach"
	KindRemediation = "remediation"

	OutboundNone      = "none"
	OutboundPending   = "pending"
	OutboundDelivered = "delivered"
	OutboundFailed    = "failed"
)

// Record is one audited request of an external agent.
type Record struct {
	ID               string       `json:"id"`
	Kind             string       `json:"kind"`
	TokenID          string       `json:"token_id"`
	IdentityName     string       `json:"identity_name"`
	Actor            string       `json:"actor"`
	Transport        string       `json:"transport"`
	Database         string       `json:"database"`
	InvestigationID  string       `json:"investigation_id,omitempty"`
	Created          bool         `json:"created"`
	Match            string       `json:"match,omitempty"`
	Symptom          *Symptom     `json:"symptom,omitempty"`
	Window           *Window      `json:"window,omitempty"`
	ExternalRef      *ExternalRef `json:"external_ref,omitempty"`
	RemediationID    string       `json:"remediation_id,omitempty"`
	Verdict          string       `json:"verdict,omitempty"`
	Reason           string       `json:"reason,omitempty"`
	Outbound         string       `json:"outbound"`
	OutboundAttempts int          `json:"outbound_attempts"`
	OutboundNextAt   time.Time    `json:"outbound_next_at"`
	OutboundError    string       `json:"outbound_error,omitempty"`
	CreatedAt        time.Time    `json:"created_at"`
}

// LiveRef is an investigation an identity opened that was not yet seen
// terminal.
type LiveRef struct {
	RecordID        string
	Database        string
	InvestigationID string
}

// RequestStore keeps the audit trail and the outbound queue.
type RequestStore interface {
	Record(ctx context.Context, r Record) (Record, error)
	LiveOpened(ctx context.Context, tokenID string) ([]LiveRef, error)
	MarkTerminal(ctx context.Context, recordID string) error
	ForInvestigation(ctx context.Context, tokenID, database, invID string) (*Record, error)
	HasExternal(ctx context.Context, system, id string) (bool, error)
	Recent(ctx context.Context, limit int) ([]Record, error)
	ClaimOutbound(ctx context.Context, now time.Time, lease time.Duration,
		limit int) ([]Record, error)
	FinishOutbound(ctx context.Context, id, state, errText string, next time.Time) error
	RescheduleOutbound(ctx context.Context, id string, next time.Time) error
}

// PGStore is the RequestStore on the control database.
type PGStore struct{ pool *pgxpool.Pool }

// NewPGStore keeps requests in sage.specialist_requests.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

const recordColumns = `id::text, kind, token_id, identity_name, actor, transport,
	database_name, COALESCE(investigation_id, ''), created, match, symptom, time_window,
	external_ref, remediation_id, verdict, reason, outbound, outbound_attempts,
	outbound_next_at, outbound_error, created_at`

func jsonOrNil(v any, isNil bool) ([]byte, error) {
	if isNil {
		return nil, nil
	}
	return json.Marshal(v)
}

// Record inserts r and returns it with its id and time.
func (s *PGStore) Record(ctx context.Context, r Record) (Record, error) {
	sym, err := jsonOrNil(r.Symptom, r.Symptom == nil)
	if err != nil {
		return Record{}, fmt.Errorf("encode symptom: %w", err)
	}
	win, err := jsonOrNil(r.Window, r.Window == nil)
	if err != nil {
		return Record{}, fmt.Errorf("encode window: %w", err)
	}
	ext, err := jsonOrNil(r.ExternalRef, r.ExternalRef == nil)
	if err != nil {
		return Record{}, fmt.Errorf("encode external reference: %w", err)
	}
	if r.Outbound == "" {
		r.Outbound = OutboundNone
	}
	row := s.pool.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.specialist_requests
		(kind, token_id, identity_name, actor, transport, database_name, investigation_id,
		 created, match, symptom, time_window, external_ref, remediation_id, verdict,
		 reason, outbound)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, $10, $11, $12, $13, $14,
		 $15, $16)
		RETURNING `+recordColumns, r.Kind, r.TokenID, r.IdentityName, r.Actor, r.Transport,
		r.Database, r.InvestigationID, r.Created, r.Match, sym, win, ext, r.RemediationID,
		r.Verdict, r.Reason, r.Outbound)
	out, err := scanRecord(row)
	if err != nil {
		return Record{}, fmt.Errorf("record specialist request: %w", err)
	}
	return out, nil
}

func scanRecord(row pgx.Row) (Record, error) {
	var r Record
	var sym, win, ext []byte
	err := row.Scan(&r.ID, &r.Kind, &r.TokenID, &r.IdentityName, &r.Actor, &r.Transport,
		&r.Database, &r.InvestigationID, &r.Created, &r.Match, &sym, &win, &ext,
		&r.RemediationID, &r.Verdict, &r.Reason, &r.Outbound, &r.OutboundAttempts,
		&r.OutboundNextAt, &r.OutboundError, &r.CreatedAt)
	if err != nil {
		return Record{}, err
	}
	for _, f := range []struct {
		raw    []byte
		target any
	}{{sym, &r.Symptom}, {win, &r.Window}, {ext, &r.ExternalRef}} {
		if len(f.raw) > 0 {
			if err := json.Unmarshal(f.raw, f.target); err != nil {
				return Record{}, fmt.Errorf("decode stored request: %w", err)
			}
		}
	}
	return r, nil
}

func scanRecords(rows pgx.Rows) ([]Record, error) {
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LiveOpened lists the investigations tokenID ("" = every token) opened
// that were not yet seen terminal.
func (s *PGStore) LiveOpened(ctx context.Context, tokenID string) ([]LiveRef, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT id::text, database_name,
		COALESCE(investigation_id, '') FROM sage.specialist_requests
		WHERE kind = 'open' AND created AND terminal_at IS NULL
		  AND ($1 = '' OR token_id = $1)
		ORDER BY created_at LIMIT 1000`, tokenID)
	if err != nil {
		return nil, fmt.Errorf("list live specialist investigations: %w", err)
	}
	defer rows.Close()
	var out []LiveRef
	for rows.Next() {
		var l LiveRef
		if err := rows.Scan(&l.RecordID, &l.Database, &l.InvestigationID); err != nil {
			return nil, fmt.Errorf("scan live specialist investigation: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MarkTerminal records that a record's investigation finished.
func (s *PGStore) MarkTerminal(ctx context.Context, recordID string) error {
	_, err := s.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.specialist_requests
		SET terminal_at = now(), updated_at = now()
		WHERE id = $1::uuid AND terminal_at IS NULL`, recordID)
	if err != nil {
		return fmt.Errorf("mark specialist request %s terminal: %w", recordID, err)
	}
	return nil
}

// ForInvestigation is the caller's latest open/attach record of an
// investigation, nil when there is none.
func (s *PGStore) ForInvestigation(ctx context.Context, tokenID, database,
	invID string) (*Record, error) {
	r, err := scanRecord(s.pool.QueryRow(ctx, `/* pg_sage */ SELECT `+recordColumns+`
		FROM sage.specialist_requests
		WHERE token_id = $1 AND database_name = $2 AND investigation_id = $3
		  AND kind IN ('open', 'attach')
		ORDER BY created_at DESC LIMIT 1`, tokenID, database, invID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the caller's specialist request: %w", err)
	}
	return &r, nil
}

// HasExternal reports whether a result post was already queued for an
// external reference.
func (s *PGStore) HasExternal(ctx context.Context, system, id string) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (SELECT 1
		FROM sage.specialist_requests
		WHERE external_ref->>'system' = $1 AND external_ref->>'id' = $2
		  AND outbound <> 'none')`, system, id).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("check queued result posts: %w", err)
	}
	return found, nil
}

// Recent lists the newest records.
func (s *PGStore) Recent(ctx context.Context, limit int) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT `+recordColumns+`
		FROM sage.specialist_requests ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list specialist requests: %w", err)
	}
	return scanRecords(rows)
}
