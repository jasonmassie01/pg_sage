package schema

// MCP v2 for coding agents (roadmap phase 3). All idempotent.
//
// sage.mcp_tokens holds the scoped API tokens MCP clients authenticate
// with (control database). Only the SHA-256 of a token is stored, with a
// short prefix to recognize it. Scopes are read, propose and approve;
// approve is only ever held by an operator token bound to a person
// (owner_user_id), never by an agent token: the CHECK repeats the rule the
// store enforces. databases is the list of fleet databases the token may
// name ('{*}' for every database).
//
// sage.source_fix records what a coding agent reported about the
// source-fix packet of a finding (monitored database), one row per
// finding with the packet hash it was opened from: the pull request,
// the deploy, and pg_sage's verdict after it (predicted vs observed). A
// decided verdict is never rewritten.
const ddlMCPv2 = `
CREATE TABLE IF NOT EXISTS sage.mcp_tokens (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    kind          text NOT NULL CHECK (kind IN ('agent', 'operator')),
    scopes        text[] NOT NULL CHECK (scopes <@ ARRAY['read', 'propose', 'approve']
                      AND 'read' = ANY (scopes)),
    databases     text[] NOT NULL CHECK (cardinality(databases) >= 1),
    token_hash    text NOT NULL UNIQUE CHECK (length(token_hash) BETWEEN 32 AND 128),
    prefix        text NOT NULL CHECK (length(prefix) BETWEEN 1 AND 16),
    owner_user_id integer REFERENCES sage.users(id) ON DELETE CASCADE,
    created_by    text NOT NULL CHECK (length(created_by) BETWEEN 1 AND 200),
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    revoked_by    text CHECK (length(revoked_by) <= 200),
    last_used_at  timestamptz,
    CHECK (kind = 'operator' OR NOT ('approve' = ANY (scopes))),
    CHECK (kind <> 'operator' OR owner_user_id IS NOT NULL),
    CHECK (kind <> 'agent' OR owner_user_id IS NULL),
    CHECK (expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS mcp_tokens_created
    ON sage.mcp_tokens (created_at DESC);
CREATE INDEX IF NOT EXISTS mcp_tokens_owner
    ON sage.mcp_tokens (owner_user_id);
CREATE TABLE IF NOT EXISTS sage.source_fix (
    id               bigserial PRIMARY KEY,
    finding_id       bigint NOT NULL CHECK (finding_id > 0),
    packet_hash      text NOT NULL CHECK (length(packet_hash) BETWEEN 1 AND 128),
    stage            text NOT NULL CHECK (stage IN ('pr_opened', 'deployed', 'verified')),
    pr_url           text CHECK (length(pr_url) <= 2048),
    commit_sha       text CHECK (commit_sha ~ '^[0-9a-f]{7,64}$'),
    reported_by      text NOT NULL CHECK (length(reported_by) BETWEEN 1 AND 200),
    deployed_at      timestamptz,
    prediction       jsonb NOT NULL DEFAULT '{}'::jsonb,
    target_queryids  bigint[] NOT NULL DEFAULT '{}',
    verdict          text CHECK (verdict IN ('improved', 'neutral', 'regressed',
                         'insufficient_evidence', 'unverifiable')),
    tolerance        text,
    observed         jsonb,
    evidence         jsonb,
    reason           text NOT NULL DEFAULT '' CHECK (length(reason) <= 2000),
    decided_at       timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CHECK ((verdict IS NULL) = (decided_at IS NULL)),
    CHECK (stage <> 'pr_opened' OR deployed_at IS NULL)
);
CREATE UNIQUE INDEX IF NOT EXISTS source_fix_finding
    ON sage.source_fix (finding_id);
`
