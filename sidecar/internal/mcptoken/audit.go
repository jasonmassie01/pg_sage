package mcptoken

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/auth"
)

// Token audit (agentgov-05): issuing and revoking a token writes one
// chained sage.auth_audit row in the change's own transaction, so a failed
// audit write fails the change. target_user_id is an operator token's
// owner, or the sponsor of an agent token's principal (0 when it has
// none). The detail never carries the secret, its hash or its prefix.

func recordCreated(ctx context.Context, tx pgx.Tx, a Audit, tok Token) error {
	event := auth.AuditMCPTokenCreated
	if tok.Kind == KindAgent {
		event = auth.AuditAgentTokenMinted
	}
	return recordToken(ctx, tx, a, event, tok)
}

func recordRevoked(ctx context.Context, tx pgx.Tx, a Audit, tok Token) error {
	return recordToken(ctx, tx, a, auth.AuditMCPTokenRevoked, tok)
}

func recordToken(ctx context.Context, tx pgx.Tx, a Audit, event string, tok Token) error {
	target, err := auditTarget(ctx, tx, tok)
	if err != nil {
		return err
	}
	detail := map[string]any{"token_id": tok.ID, "name": tok.Name, "kind": string(tok.Kind),
		"scopes": tok.Scopes, "databases": tok.Databases, "expires_at": tok.ExpiresAt}
	if tok.PrincipalID != "" {
		detail["principal_id"] = tok.PrincipalID
	}
	if tok.OwnerUserID != nil {
		detail["owner_user_id"] = *tok.OwnerUserID
	}
	err = auth.RecordAuthAudit(ctx, tx, auth.AuthAuditEvent{Event: event,
		ActorUserID: a.ActorUserID, TargetUserID: target, SourceIP: a.SourceIP,
		Detail: detail})
	if err != nil {
		return fmt.Errorf("mcptoken: %w; the token was not changed", err)
	}
	return nil
}

// auditTarget is the owner of an operator token or the sponsor of an
// agent token's principal.
func auditTarget(ctx context.Context, tx pgx.Tx, tok Token) (int, error) {
	if tok.OwnerUserID != nil {
		return *tok.OwnerUserID, nil
	}
	if tok.PrincipalID == "" {
		return 0, nil
	}
	var sponsor int
	err := tx.QueryRow(ctx, `/* pg_sage mcp_token_audit_target v1 */
		SELECT COALESCE((SELECT sponsor_user_id FROM sage.guard_principals
		WHERE id = $1), 0)`, tok.PrincipalID).Scan(&sponsor)
	if err != nil {
		return 0, fmt.Errorf("mcptoken: reading the sponsor of %s: %w", tok.PrincipalID, err)
	}
	return sponsor, nil
}
