package agentdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	pingTokenFailureLimit  = 5
	pingTokenFailureWindow = 5 * time.Minute
)

func (s *Store) UpsertAgentIdentity(
	ctx context.Context,
	req AgentIdentityRequest,
) (AgentIdentity, error) {
	if err := s.Ensure(ctx); err != nil {
		return AgentIdentity{}, err
	}
	req.AgentID = strings.TrimSpace(req.AgentID)
	req.TenantID = strings.TrimSpace(req.TenantID)
	if req.AgentID == "" || req.TenantID == "" {
		return AgentIdentity{}, ErrInvalid
	}
	if req.Status == "" {
		req.Status = "active"
	}
	var identity AgentIdentity
	err := scanAgentIdentity(s.pool.QueryRow(ctx, `/* pg_sage */ 
		INSERT INTO sage.agent_identities (
			agent_id, tenant_id, owner_id, display_name, status, metadata
		)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		ON CONFLICT (agent_id) DO UPDATE
		SET tenant_id=EXCLUDED.tenant_id,
			owner_id=EXCLUDED.owner_id,
			display_name=EXCLUDED.display_name,
			status=EXCLUDED.status,
			metadata=EXCLUDED.metadata,
			updated_at=now()
		RETURNING agent_id, tenant_id, owner_id, display_name, status,
			metadata, created_at, updated_at`,
		req.AgentID,
		req.TenantID,
		req.OwnerID,
		req.DisplayName,
		req.Status,
		jsonBytes(req.Metadata),
	), &identity)
	return identity, err
}

func (s *Store) AgentIdentities(ctx context.Context) ([]AgentIdentity, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ 
		SELECT agent_id, tenant_id, owner_id, display_name, status,
			metadata, created_at, updated_at
		FROM sage.agent_identities
		ORDER BY updated_at DESC, agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentIdentity{}
	for rows.Next() {
		var identity AgentIdentity
		if err := scanAgentIdentity(rows, &identity); err != nil {
			return nil, err
		}
		out = append(out, identity)
	}
	return out, rows.Err()
}

func (s *Store) CreatePingToken(
	ctx context.Context,
	id string,
	req PingTokenRequest,
) (PingToken, error) {
	if err := s.Ensure(ctx); err != nil {
		return PingToken{}, err
	}
	dep, err := s.Get(ctx, id)
	if err != nil {
		return PingToken{}, err
	}
	if req.AgentID == "" {
		req.AgentID = dep.AgentID
	}
	if req.AgentID != dep.AgentID || req.ExpiresSeconds <= 0 {
		return PingToken{}, ErrInvalid
	}
	token, err := randomToken()
	if err != nil {
		return PingToken{}, err
	}
	tokenID := "pt_" + idFrom(id, token)
	var out PingToken
	err = scanPingToken(s.pool.QueryRow(ctx, `/* pg_sage */ 
		INSERT INTO sage.agent_db_ping_tokens (
			token_id, deployment_id, agent_id, token_hash, scope,
			expires_at, rotated_from_token_id
		)
		VALUES ($1, $2, $3, $4, 'ping', now()+make_interval(secs => $5), $6)
		RETURNING token_id, deployment_id, agent_id, token_hash, scope, status,
			expires_at, created_at, last_used_at, revoked_at, rotated_from_token_id,
			0 AS failed_attempts`,
		tokenID, id, req.AgentID, tokenHash(token), req.ExpiresSeconds,
		req.RotatedFromTokenID,
	), &out)
	if err != nil {
		return PingToken{}, err
	}
	_ = s.audit(ctx, id, "ping_token_created", map[string]any{
		"token_id": tokenID,
		"agent_id": req.AgentID,
	})
	out.Token = token
	out.TokenHash = ""
	return out, nil
}

func (s *Store) PingTokens(ctx context.Context, id string) ([]PingToken, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ 
		SELECT token_id, deployment_id, agent_id, token_hash, scope, status,
			expires_at, created_at, last_used_at, revoked_at, rotated_from_token_id,
			(
				SELECT count(*)::int
				FROM sage.agent_db_ping_token_failures f
				WHERE f.deployment_id=sage.agent_db_ping_tokens.deployment_id
					AND f.token_hash=sage.agent_db_ping_tokens.token_hash
					AND f.created_at > now()-interval '24 hours'
			) AS failed_attempts
		FROM sage.agent_db_ping_tokens
		WHERE deployment_id=$1
		ORDER BY created_at DESC, token_id`,
		id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PingToken{}
	for rows.Next() {
		var token PingToken
		if err := scanPingToken(rows, &token); err != nil {
			return nil, err
		}
		redactPingToken(&token)
		out = append(out, token)
	}
	return out, rows.Err()
}

func (s *Store) RevokePingToken(
	ctx context.Context,
	id string,
	tokenID string,
	reason string,
) (PingToken, error) {
	if err := s.Ensure(ctx); err != nil {
		return PingToken{}, err
	}
	var out PingToken
	err := scanPingToken(s.pool.QueryRow(ctx, `/* pg_sage */ 
		UPDATE sage.agent_db_ping_tokens
		SET status='revoked', revoked_at=COALESCE(revoked_at, now())
		WHERE deployment_id=$1 AND token_id=$2
		RETURNING token_id, deployment_id, agent_id, token_hash, scope, status,
			expires_at, created_at, last_used_at, revoked_at, rotated_from_token_id,
			0 AS failed_attempts`,
		id, strings.TrimSpace(tokenID),
	), &out)
	if errors.Is(err, pgx.ErrNoRows) {
		return PingToken{}, ErrNotFound
	}
	if err != nil {
		return PingToken{}, err
	}
	_ = s.audit(ctx, id, "ping_token_revoked", map[string]any{
		"token_id": tokenID,
		"reason":   reason,
	})
	redactPingToken(&out)
	return out, nil
}

func (s *Store) RotatePingToken(
	ctx context.Context,
	id string,
	tokenID string,
	req PingTokenRequest,
) (PingToken, error) {
	old, err := s.pingToken(ctx, id, tokenID)
	if err != nil {
		return PingToken{}, err
	}
	if old.Status != "active" || old.ExpiresAt.Before(time.Now().UTC()) {
		return PingToken{}, ErrNotFound
	}
	if req.ExpiresSeconds <= 0 {
		req.ExpiresSeconds = int(time.Until(old.ExpiresAt).Seconds())
	}
	req.AgentID = old.AgentID
	req.RotatedFromTokenID = old.TokenID
	created, err := s.CreatePingToken(ctx, id, req)
	if err != nil {
		return PingToken{}, err
	}
	if _, err := s.RevokePingToken(ctx, id, old.TokenID, "rotated"); err != nil {
		return PingToken{}, err
	}
	_ = s.audit(ctx, id, "ping_token_rotated", map[string]any{
		"old_token_id": old.TokenID,
		"new_token_id": created.TokenID,
	})
	return created, nil
}

func (s *Store) ValidatePingToken(
	ctx context.Context,
	id string,
	token string,
) (PingToken, error) {
	if err := s.Ensure(ctx); err != nil {
		return PingToken{}, err
	}
	var out PingToken
	err := scanPingToken(s.pool.QueryRow(ctx, `/* pg_sage */ 
		UPDATE sage.agent_db_ping_tokens
		SET last_used_at=now()
		WHERE deployment_id=$1
			AND token_hash=$2
			AND scope='ping'
			AND status='active'
			AND expires_at > now()
		RETURNING token_id, deployment_id, agent_id, token_hash, scope, status,
			expires_at, created_at, last_used_at, revoked_at, rotated_from_token_id,
			0 AS failed_attempts`,
		id, tokenHash(token),
	), &out)
	if errors.Is(err, pgx.ErrNoRows) {
		return PingToken{}, s.recordPingTokenFailure(ctx, id, token, "not_found")
	}
	if err != nil {
		return PingToken{}, err
	}
	redactPingToken(&out)
	return out, nil
}

func (s *Store) AgentPing(
	ctx context.Context,
	id string,
	token string,
	req PingRequest,
) (Deployment, error) {
	if _, err := s.ValidatePingToken(ctx, id, token); err != nil {
		return Deployment{}, err
	}
	return s.Ping(ctx, id, req)
}

func (s *Store) pingToken(ctx context.Context, id, tokenID string) (PingToken, error) {
	if err := s.Ensure(ctx); err != nil {
		return PingToken{}, err
	}
	var out PingToken
	err := scanPingToken(s.pool.QueryRow(ctx, `/* pg_sage */ 
		SELECT token_id, deployment_id, agent_id, token_hash, scope, status,
			expires_at, created_at, last_used_at, revoked_at, rotated_from_token_id,
			0 AS failed_attempts
		FROM sage.agent_db_ping_tokens
		WHERE deployment_id=$1 AND token_id=$2`,
		id, strings.TrimSpace(tokenID),
	), &out)
	if errors.Is(err, pgx.ErrNoRows) {
		return PingToken{}, ErrNotFound
	}
	if err != nil {
		return PingToken{}, err
	}
	return out, nil
}

func (s *Store) recordPingTokenFailure(
	ctx context.Context,
	id string,
	token string,
	reason string,
) error {
	hash := tokenHash(token)
	if _, err := s.pool.Exec(ctx, `/* pg_sage */ 
		INSERT INTO sage.agent_db_ping_token_failures (
			deployment_id, token_hash, reason
		)
		VALUES ($1, $2, $3)`,
		id, hash, reason,
	); err != nil {
		return err
	}
	_ = s.audit(ctx, id, "ping_token_failed", map[string]any{
		"hash_prefix": hashPrefix(hash),
		"reason":      reason,
	})
	var failures int
	err := s.pool.QueryRow(ctx, `/* pg_sage */ 
		SELECT count(*)::int
		FROM sage.agent_db_ping_token_failures
		WHERE deployment_id=$1 AND token_hash=$2 AND created_at > $3`,
		id, hash, time.Now().UTC().Add(-pingTokenFailureWindow),
	).Scan(&failures)
	if err != nil {
		return err
	}
	if failures >= pingTokenFailureLimit {
		return ErrRateLimited
	}
	return ErrNotFound
}

func randomToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func hashPrefix(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func redactPingToken(token *PingToken) {
	token.Token = ""
	token.TokenHash = ""
}

func scanAgentIdentity(row scanner, identity *AgentIdentity) error {
	return row.Scan(
		&identity.AgentID,
		&identity.TenantID,
		&identity.OwnerID,
		&identity.DisplayName,
		&identity.Status,
		&identity.Metadata,
		&identity.CreatedAt,
		&identity.UpdatedAt,
	)
}

func scanPingToken(row scanner, token *PingToken) error {
	return row.Scan(
		&token.TokenID,
		&token.DeploymentID,
		&token.AgentID,
		&token.TokenHash,
		&token.Scope,
		&token.Status,
		&token.ExpiresAt,
		&token.CreatedAt,
		&token.LastUsedAt,
		&token.RevokedAt,
		&token.RotatedFromTokenID,
		&token.FailedAttempts,
	)
}
