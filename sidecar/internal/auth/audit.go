package auth

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Account-linking audit events (D7).
const (
	AuditOIDCLinked          = "oidc_linked"
	AuditOIDCUnlinked        = "oidc_unlinked"
	AuditOIDCLinkGrantIssued = "oidc_link_grant_issued"
	AuditOIDCLinkGrantUsed   = "oidc_link_grant_used"
)

// AuthAuditEvent is one identity-change record. Detail must never carry
// credentials, grant tokens or email addresses.
type AuthAuditEvent struct {
	Event        string
	ActorUserID  int
	TargetUserID int
	Detail       map[string]any
}

// RecordAuthAudit appends ev to sage.auth_audit.
func RecordAuthAudit(
	ctx context.Context, pool *pgxpool.Pool, ev AuthAuditEvent,
) error {
	detail := ev.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	body, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encoding auth audit detail: %w", err)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO sage.auth_audit "+
			"(event, actor_user_id, target_user_id, detail) "+
			"VALUES ($1, $2, $3, $4::jsonb)",
		ev.Event, ev.ActorUserID, ev.TargetUserID, string(body)); err != nil {
		return fmt.Errorf("recording auth audit %s: %w", ev.Event, err)
	}
	return nil
}
