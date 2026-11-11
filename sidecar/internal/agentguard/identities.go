package agentguard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/auth"
)

// External identity bindings (agentgov-03). An OAuth identity, the pair
// (issuer, subject), maps to one principal through
// sage.guard_identity_bindings, which the MCP OAuth resolver reads. A
// binding grants exactly the principal's profile and ceiling, so it is
// treated like minting a token: one admin, recorded as
// agent_identity_bound in the insert's transaction (a failed audit write
// fails the bind). Unbinding narrows and is allowed in any status.

// MaxSubjectBytes is the longest subject a binding accepts.
const MaxSubjectBytes = 255

const (
	maxIssuerBytes = 2000
	// maxIdentities caps one principal's listing; more is reported as
	// truncated.
	maxIdentities     = 500
	sqlUndefinedTable = "42P01"
)

var (
	// ErrIdentityBound is an (issuer, subject) that is already bound, to
	// this principal or another (409).
	ErrIdentityBound = errors.New(
		"agentguard: this identity is already bound to an agent principal")
	// ErrIdentityNotBound is an unbind of an identity this principal does
	// not hold (404).
	ErrIdentityNotBound = errors.New(
		"agentguard: this identity is not bound to the agent principal")
)

// IdentityBinding is one bound OAuth identity.
type IdentityBinding struct {
	Issuer      string    `json:"issuer"`
	Subject     string    `json:"subject"`
	PrincipalID string    `json:"principal_id"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// IdentityList is a principal's bindings, oldest first.
type IdentityList struct {
	Items     []IdentityBinding `json:"items"`
	Truncated bool              `json:"truncated"`
}

// BindRequest describes an identity to bind. The caller has checked the
// issuer against the configured issuers; the store checks its shape.
type BindRequest struct {
	Issuer    string
	Subject   string
	CreatedBy string
	Audit     Audit // who bound it (Event is not used)
}

// checkIdentity requires a non-empty issuer and subject of valid UTF-8,
// without control characters, within their byte limits.
func checkIdentity(issuer, subject string) error {
	if err := checkIdentityText("issuer", issuer, maxIssuerBytes); err != nil {
		return err
	}
	return checkIdentityText("subject", subject, MaxSubjectBytes)
}

func checkIdentityText(field, value string, max int) error {
	if value == "" || len(value) > max || !utf8.ValidString(value) {
		return invalid("%s must be 1 to %d bytes of UTF-8", field, max)
	}
	if strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return invalid("%s must not contain control characters", field)
	}
	return nil
}

func (r BindRequest) validate() error {
	if err := checkIdentity(r.Issuer, r.Subject); err != nil {
		return err
	}
	if err := checkText("created_by", r.CreatedBy, 1, maxActorLen); err != nil {
		return err
	}
	return r.Audit.validate()
}

// identityDetail is what the bind and unbind events record.
func identityDetail(id, name, issuer, subject string) map[string]any {
	return map[string]any{"principal_id": id, "name": name, "issuer": issuer,
		"subject": subject}
}

// BindIdentity binds (issuer, subject) to an active principal. Unknown is
// ErrNotFound, retired ErrRetired, frozen a DeniedError (agent_frozen), a
// taken identity ErrIdentityBound.
func (s *Store) BindIdentity(ctx context.Context, principalID string,
	req BindRequest) (IdentityBinding, error) {
	if err := req.validate(); err != nil {
		return IdentityBinding{}, err
	}
	if !ValidID(principalID) {
		return IdentityBinding{}, fmt.Errorf("%w: principal %q", ErrNotFound, principalID)
	}
	if err := s.ready(); err != nil {
		return IdentityBinding{}, err
	}
	b := IdentityBinding{Issuer: req.Issuer, Subject: req.Subject,
		PrincipalID: principalID, CreatedBy: req.CreatedBy}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		l, err := lockForChange(ctx, tx, principalID)
		if err != nil {
			return err
		}
		if l.status == StatusFrozen {
			return &DeniedError{Reason: ReasonFrozen,
				Detail: "a frozen principal gets no new identity binding",
				Fix:    "unfreeze it first"}
		}
		err = tx.QueryRow(ctx, `/* pg_sage guard_identity_bind v1 */
			INSERT INTO sage.guard_identity_bindings (issuer, subject, principal_id, created_by)
			VALUES ($1, $2, $3, $4) RETURNING created_at`,
			req.Issuer, req.Subject, principalID, req.CreatedBy).Scan(&b.CreatedAt)
		if err != nil {
			return identityWriteError(err, "binding an identity to "+principalID)
		}
		return record(ctx, tx, req.Audit, auth.AuditAgentIdentityBound, l.shape.SponsorUserID,
			identityDetail(principalID, l.name, req.Issuer, req.Subject))
	})
	if err != nil {
		return IdentityBinding{}, err
	}
	return b, nil
}

// identityWriteError maps a taken identity and a missing table (a schema
// older than G1).
func identityWriteError(err error, what string) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case sqlUniqueViol:
			return ErrIdentityBound
		case sqlUndefinedTable:
			return fmt.Errorf("%w: identity bindings are not installed", ErrUnavailable)
		}
	}
	return fmt.Errorf("agentguard: %s: %w", what, err)
}

// UnbindIdentity removes (issuer, subject) from the principal, whatever
// its status, and returns the removed binding. An identity the principal
// does not hold is ErrIdentityNotBound.
func (s *Store) UnbindIdentity(ctx context.Context, principalID, issuer, subject string,
	a Audit) (IdentityBinding, error) {
	if err := checkIdentity(issuer, subject); err != nil {
		return IdentityBinding{}, err
	}
	if err := a.validate(); err != nil {
		return IdentityBinding{}, err
	}
	if !ValidID(principalID) {
		return IdentityBinding{}, fmt.Errorf("%w: principal %q", ErrNotFound, principalID)
	}
	if err := s.ready(); err != nil {
		return IdentityBinding{}, err
	}
	b := IdentityBinding{Issuer: issuer, Subject: subject, PrincipalID: principalID}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		name, sponsor, err := lockAnyStatus(ctx, tx, principalID)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `/* pg_sage guard_identity_unbind v1 */
			DELETE FROM sage.guard_identity_bindings
			WHERE issuer = $1 AND subject = $2 AND principal_id = $3
			RETURNING created_by, created_at`, issuer, subject, principalID).
			Scan(&b.CreatedBy, &b.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrIdentityNotBound
		}
		if err != nil {
			return identityWriteError(err, "unbinding an identity from "+principalID)
		}
		return record(ctx, tx, a, auth.AuditAgentIdentityUnbound, sponsor,
			identityDetail(principalID, name, issuer, subject))
	})
	if err != nil {
		return IdentityBinding{}, err
	}
	return b, nil
}

// lockAnyStatus locks the principal's row for tx in any status and reads
// its name and sponsor. Unknown is ErrNotFound.
func lockAnyStatus(ctx context.Context, tx pgx.Tx, id string) (string, *int, error) {
	var name string
	var sponsor *int
	err := tx.QueryRow(ctx, `/* pg_sage guard_principal_lock_any v1 */
		SELECT name, sponsor_user_id FROM sage.guard_principals WHERE id = $1 FOR UPDATE`,
		id).Scan(&name, &sponsor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, fmt.Errorf("%w: principal %q", ErrNotFound, id)
	}
	if err != nil {
		return "", nil, fmt.Errorf("agentguard: locking principal %s: %w", id, err)
	}
	return name, sponsor, nil
}

// ListIdentities lists the principal's bindings, oldest first, at most
// maxIdentities of them. Unknown is ErrNotFound.
func (s *Store) ListIdentities(ctx context.Context, principalID string) (IdentityList,
	error) {
	if _, err := s.Get(ctx, principalID); err != nil {
		return IdentityList{}, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage guard_identity_list v1 */
		SELECT issuer, subject, principal_id, created_by, created_at
		FROM sage.guard_identity_bindings WHERE principal_id = $1
		ORDER BY created_at, issuer, subject LIMIT $2`, principalID, maxIdentities+1)
	if err != nil {
		return IdentityList{}, identityWriteError(err, "listing the identities of "+
			principalID)
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[IdentityBinding])
	if err != nil {
		return IdentityList{}, identityWriteError(err, "reading the identities of "+
			principalID)
	}
	out := IdentityList{Items: append([]IdentityBinding{}, items...)}
	if len(items) > maxIdentities {
		out.Items, out.Truncated = items[:maxIdentities], true
	}
	return out, nil
}
