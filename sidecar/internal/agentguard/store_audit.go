package agentguard

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/auth"
)

// Governance audit (agentgov-05). Every Create, Update and SetStatus
// writes one chained sage.auth_audit row in the change's own transaction,
// so a failed audit write fails the change: governance changes are rare
// and trust-critical. target_user_id is the sponsor.

// Audit says who made a governance change, from where and why.
type Audit struct {
	ActorUserID int    // the signed-in admin; 0 when no user made it
	SourceIP    string // the client address as the API resolved it
	// Event overrides the method's own event: Update also records
	// auth.AuditAgentWidenedSingleOperator. "" = the method's event.
	Event  string
	Reason string // kept in the event; SetStatus also keeps it as frozen_reason
}

const maxSourceIPLen = 200

// validate checks the audit's shape; a non-empty Event must be one of
// allowed.
func (a Audit) validate(allowed ...string) error {
	if a.ActorUserID < 0 {
		return invalid("audit actor_user_id must not be negative")
	}
	if err := checkText("source_ip", a.SourceIP, 0, maxSourceIPLen); err != nil {
		return err
	}
	if err := checkText("reason", a.Reason, 0, maxReasonLen); err != nil {
		return err
	}
	if a.Event == "" {
		return nil
	}
	for _, e := range allowed {
		if a.Event == e {
			return nil
		}
	}
	return invalid("audit event %q does not record this change", a.Event)
}

// event is a.Event, or def when it is unset.
func (a Audit) event(def string) string {
	if a.Event == "" {
		return def
	}
	return a.Event
}

// shape is the governed part of a principal, as events record it.
type shape struct {
	SponsorUserID *int   `json:"sponsor_user_id"`
	Profile       string `json:"profile"`
	EnvCeiling    string `json:"env_ceiling"`
}

// record appends the event to sage.auth_audit inside tx.
func record(ctx context.Context, tx pgx.Tx, a Audit, event string, sponsor *int,
	detail map[string]any) error {
	target := 0
	if sponsor != nil {
		target = *sponsor
	}
	err := auth.RecordAuthAudit(ctx, tx, auth.AuthAuditEvent{Event: event,
		ActorUserID: a.ActorUserID, TargetUserID: target, SourceIP: a.SourceIP,
		Detail: detail})
	if err != nil {
		return fmt.Errorf("agentguard: %w; the change was not made", err)
	}
	return nil
}

// checkSponsor requires user id to be an enabled operator or admin and
// holds a share lock on the user row for the rest of tx, so a concurrent
// demotion waits for the change (or the change sees it).
func checkSponsor(ctx context.Context, tx pgx.Tx, id int) error {
	var eligible bool
	err := tx.QueryRow(ctx, `/* pg_sage guard_sponsor_check v1 */
		SELECT role IN ('operator', 'admin') AND disabled_at IS NULL
		FROM sage.users WHERE id = $1 FOR SHARE`, id).Scan(&eligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: user %d", ErrSponsorNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("agentguard: reading sponsor user %d: %w", id, err)
	}
	if !eligible {
		return fmt.Errorf("%w (user %d)", ErrSponsorNotEligible, id)
	}
	return nil
}

// lockedPrincipal is a principal row locked for a change.
type lockedPrincipal struct {
	name   string
	status Status
	shape  shape
}

// lockForChange locks the principal's row for tx. Unknown is ErrNotFound;
// retired, which never changes again, is ErrRetired.
func lockForChange(ctx context.Context, tx pgx.Tx, id string) (lockedPrincipal, error) {
	var l lockedPrincipal
	var status string
	err := tx.QueryRow(ctx, `/* pg_sage guard_principal_lock v1 */
		SELECT name, status, sponsor_user_id, profile, env_ceiling
		FROM sage.guard_principals WHERE id = $1 FOR UPDATE`, id).
		Scan(&l.name, &status, &l.shape.SponsorUserID, &l.shape.Profile, &l.shape.EnvCeiling)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, fmt.Errorf("%w: principal %q", ErrNotFound, id)
	}
	if err != nil {
		return l, fmt.Errorf("agentguard: locking principal %s: %w", id, err)
	}
	l.status = Status(status)
	if l.status == StatusRetired {
		return l, fmt.Errorf("%w: principal %s", ErrRetired, id)
	}
	return l, nil
}

// refuseWhileFrozen is ErrSponsorChangeFrozen while the principal has an
// open freeze. The caller holds the principal's row lock, which a kill
// takes before it writes the freeze, so the two serialize.
func refuseWhileFrozen(ctx context.Context, tx pgx.Tx, id string) error {
	var frozen bool
	err := tx.QueryRow(ctx, `/* pg_sage guard_sponsor_freeze_check v1 */
		SELECT EXISTS (SELECT 1 FROM sage.guard_freezes
		WHERE scope = 'principal' AND target = $1 AND cleared_at IS NULL)`, id).Scan(&frozen)
	if err != nil {
		return fmt.Errorf("agentguard: reading the freezes of %s: %w", id, err)
	}
	if frozen {
		return fmt.Errorf("%w: principal %s", ErrSponsorChangeFrozen, id)
	}
	return nil
}
