package agentdb

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	agentTokenPrefix     = "agt_"
	maxAgentTokenSeconds = 90 * 24 * 3600
)

// AgentPrincipal is an authenticated agent. Its tenant and agent id come
// from the agent_identities row the token was minted for, never from a
// request body (G8-B05).
type AgentPrincipal struct {
	TokenID  string `json:"token_id"`
	TenantID string `json:"tenant_id"`
	AgentID  string `json:"agent_id"`
}

type AgentToken struct {
	TokenID   string    `json:"token_id"`
	TenantID  string    `json:"tenant_id"`
	AgentID   string    `json:"agent_id"`
	Token     string    `json:"token,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreateAgentToken mints a tenant-bound agent API token for an active
// identity. The plaintext is returned once; only its hash is stored.
func (s *Store) CreateAgentToken(
	ctx context.Context,
	agentID string,
	createdBy string,
	expiresSeconds int,
) (AgentToken, error) {
	if err := s.Ensure(ctx); err != nil {
		return AgentToken{}, err
	}
	if expiresSeconds <= 0 || expiresSeconds > maxAgentTokenSeconds ||
		strings.TrimSpace(createdBy) == "" {
		return AgentToken{}, ErrInvalid
	}
	raw, err := randomToken()
	if err != nil {
		return AgentToken{}, err
	}
	token := agentTokenPrefix + raw
	out := AgentToken{TokenID: "agt_id_" + idFrom(agentID, token), Token: token}
	err = s.pool.QueryRow(ctx, `/* pg_sage */
		INSERT INTO sage.agent_db_agent_tokens
			(token_id, tenant_id, agent_id, token_hash, created_by, expires_at)
		SELECT $1, tenant_id, agent_id, $3, $4, now()+make_interval(secs => $5)
		FROM sage.agent_identities WHERE agent_id=$2 AND status='active'
		RETURNING tenant_id, agent_id, expires_at`,
		out.TokenID, strings.TrimSpace(agentID), tokenHash(token), createdBy, expiresSeconds,
	).Scan(&out.TenantID, &out.AgentID, &out.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentToken{}, ErrNotFound
	}
	if err != nil {
		return AgentToken{}, err
	}
	_ = s.audit(ctx, "", "agent_token_created", map[string]any{
		"token_id": out.TokenID, "agent_id": out.AgentID, "created_by": createdBy,
	})
	return out, nil
}

// ValidateAgentToken resolves an active, unexpired token for an active
// identity into its principal.
func (s *Store) ValidateAgentToken(ctx context.Context, token string) (AgentPrincipal, error) {
	if !strings.HasPrefix(token, agentTokenPrefix) {
		return AgentPrincipal{}, ErrNotFound
	}
	if err := s.Ensure(ctx); err != nil {
		return AgentPrincipal{}, err
	}
	var out AgentPrincipal
	err := s.pool.QueryRow(ctx, `/* pg_sage */
		UPDATE sage.agent_db_agent_tokens AS token
		SET last_used_at=now()
		FROM sage.agent_identities AS identity
		WHERE token.token_hash=$1 AND token.status='active'
			AND token.expires_at > now() AND token.revoked_at IS NULL
			AND identity.agent_id=token.agent_id
			AND identity.tenant_id=token.tenant_id AND identity.status='active'
		RETURNING token.token_id, token.tenant_id, token.agent_id`,
		tokenHash(token),
	).Scan(&out.TokenID, &out.TenantID, &out.AgentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentPrincipal{}, ErrNotFound
	}
	return out, err
}

// GetForTenant returns a deployment only when it belongs to tenantID; a
// deployment of another tenant is indistinguishable from a missing one.
func (s *Store) GetForTenant(ctx context.Context, id, tenantID string) (Deployment, error) {
	dep, err := s.Get(ctx, id)
	if err != nil {
		return Deployment{}, err
	}
	if tenantID == "" || dep.TenantID != tenantID || dep.Status == "deleted" {
		return Deployment{}, ErrNotFound
	}
	return dep, nil
}

// RequestsForTenant lists the agent requests of one tenant.
func (s *Store) RequestsForTenant(ctx context.Context, tenantID string) ([]Request, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, selectRequestsSQL+`
		WHERE tenant_id=$1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Request{}
	for rows.Next() {
		var req Request
		if err := scanRequest(rows, &req); err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, rows.Err()
}

// NormalizeProviderName exposes provider alias normalization to callers that
// look up per-provider configuration.
func NormalizeProviderName(provider string) string {
	return normalizeProvider(provider)
}
