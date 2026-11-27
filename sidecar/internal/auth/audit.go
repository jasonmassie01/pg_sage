package auth

import (
	"context"
	"encoding/json"
	"fmt"
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
	// AuditUserDisabled and AuditUserEnabled record deactivation and
	// reactivation (apiauth-01); Detail carries the counts of sessions
	// ended and MCP tokens revoked.
	AuditUserDisabled = "user_disabled"
	AuditUserEnabled  = "user_enabled"
	// AuditPasswordChanged and AuditPasswordReset record a user changing
	// their own password and an admin resetting one (apiauth-03). They
	// carry the actor and target ids and a count of ended sessions only.
	AuditPasswordChanged = "password_changed"
	AuditPasswordReset   = "password_reset"
)

// Session lifecycle audit events (apiauth-04). Detail may carry a count of
// ended sessions, never a cookie or its hash.
const (
	AuditLogout          = "logout"
	AuditSessionsRevoked = "sessions_revoked"
)

// Agent governance audit events (agentgov-05). Each is written in the
// same transaction as the change it records; target_user_id is the
// agent's sponsor (or a token's owner). Detail carries principal_id, the
// before and after {sponsor_user_id, profile, env_ceiling}, the reason,
// and a token's id, name, scopes, databases and expiry; never a token
// secret or its hash.
const (
	AuditAgentCreated               = "agent_created"
	AuditAgentUpdated               = "agent_updated"
	AuditAgentWidenedSingleOperator = "agent_widened_single_operator"
	AuditAgentRetired               = "agent_retired"
	// AuditAgentStatusChanged records a freeze or unfreeze made through
	// the principal store rather than the kill switch.
	AuditAgentStatusChanged = "agent_status_changed"
	AuditAgentTokenMinted   = "agent_token_minted"
	AuditMCPTokenCreated    = "mcp_token_created"
	AuditMCPTokenRevoked    = "mcp_token_revoked"
	// AuditAgentIdentityBound and AuditAgentIdentityUnbound record an OAuth
	// (issuer, subject) bound to or removed from an agent (agentgov-03);
	// Detail carries principal_id, name, issuer and subject.
	AuditAgentIdentityBound   = "agent_identity_bound"
	AuditAgentIdentityUnbound = "agent_identity_unbound"
	// AuditAgentChangeRequested records a widening principal change waiting
	// for a second admin, and AuditAgentChangeConfirmed the second admin
	// applying it (agentgov-06). Detail carries principal_id, name,
	// change_id, before, after and reason; a confirm also carries
	// requested_by_user, a request expires_at and superseded_change_id.
	AuditAgentChangeRequested = "agent_change_requested"
	AuditAgentChangeConfirmed = "agent_change_confirmed"
)

// Databricks App identity-pin audit events (databricks-07). Detail carries
// the user id and pin prefixes only, never an email or forwarded value.
const (
	AuditDatabricksAppIdentityChanged = "databricks_app_identity_changed"
	AuditDatabricksAppPinReset        = "databricks_app_pin_reset"
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

// RecordAuthAudit appends ev to sage.auth_audit through q: a pool, or the
// transaction of the change the event records.
func RecordAuthAudit(
	ctx context.Context, q Execer, ev AuthAuditEvent,
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
	if _, err := q.Exec(ctx,
		"/* pg_sage auth_audit v1 */ INSERT INTO sage.auth_audit "+
			"(event, actor_user_id, target_user_id, detail, source_ip) "+
			"VALUES ($1, $2, $3, $4::jsonb, $5)",
		ev.Event, ev.ActorUserID, ev.TargetUserID, string(body),
		ev.SourceIP); err != nil {
		return fmt.Errorf("recording auth audit %s: %w", ev.Event, err)
	}
	return nil
}
