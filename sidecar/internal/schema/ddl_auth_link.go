package schema

// ddlAuthAccountLinking adds the D7 account-linking tables: admin-issued
// one-time SSO link grants (stored only as SHA-256 hashes) and an audit
// trail of link, unlink and grant events. Audit rows keep no foreign key so
// history survives user deletion.
const ddlAuthAccountLinking = `
CREATE TABLE IF NOT EXISTS sage.user_oidc_link_grants (
    id          bigserial PRIMARY KEY,
    user_id     INT NOT NULL REFERENCES sage.users(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    created_by  INT NOT NULL DEFAULT 0,
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_user_oidc_link_grants_user
    ON sage.user_oidc_link_grants (user_id)
    WHERE used_at IS NULL;
CREATE TABLE IF NOT EXISTS sage.auth_audit (
    id              bigserial PRIMARY KEY,
    event           TEXT NOT NULL,
    actor_user_id   INT NOT NULL DEFAULT 0,
    target_user_id  INT NOT NULL DEFAULT 0,
    detail          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_auth_audit_target
    ON sage.auth_audit (target_user_id, created_at DESC);
`
