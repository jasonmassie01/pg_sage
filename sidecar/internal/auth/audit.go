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

// Login and user-administration audit events (E1, CG-05).
const (
	AuditLoginSucceeded   = "login_succeeded"
	AuditLoginFailed      = "login_failed"
	AuditBreakGlassLogin  = "break_glass_login"
	AuditBreakGlassFailed = "break_glass_login_failed"
	AuditUserCreated      = "user_created"
	AuditUserDeleted      = "user_deleted"
	AuditUserRoleChanged  = "user_role_changed"
)

// AuthAuditEvent is one identity-change record. Detail must never carry
// credentials, grant tokens or email addresses. SourceIP is the client
// address as the API resolved it (trusted proxies honoured).
type AuthAuditEvent struct {
	Event        string
	ActorUserID  int
	TargetUserID int
	SourceIP     string
	Detail       map[string]any
}

// RecordAuthAudit appends ev to sage.auth_audit.
func RecordAuthAudit(
	ctx context.Context, pool *pgxpool.Pool, ev AuthAuditEvent,
) error {
	if ev.Event == "" {
		return fmt.Errorf("recording auth audit: event is required")
	}
	detail := ev.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	body, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encoding auth audit detail: %w", err)
	}
	if _, err := pool.Exec(ctx,
		"/* pg_sage auth_audit v1 */ INSERT INTO sage.auth_audit "+
			"(event, actor_user_id, target_user_id, detail, source_ip) "+
			"VALUES ($1, $2, $3, $4::jsonb, $5)",
		ev.Event, ev.ActorUserID, ev.TargetUserID, string(body),
		ev.SourceIP); err != nil {
		return fmt.Errorf("recording auth audit %s: %w", ev.Event, err)
	}
	return nil
}
