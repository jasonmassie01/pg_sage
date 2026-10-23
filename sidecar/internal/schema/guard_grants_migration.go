package schema

// Agent grants (AGENTDB-SPEC §6.3, §6.6, §7), in every monitored database.
// All idempotent.
//
//   - sage.guard_grants is the grant registry: one row per privilege set
//     pg_sage granted an agent role on one object, with the grantor read
//     from aclexplode after the GRANT and the expiry computed on this
//     database's clock. Columns beyond §7: object_kind and schema_oid let
//     the schema USAGE that comes with a table grant be its own row,
//     reference-counted by the table rows of its schema; revoke_detail
//     names the residue of a revoke_incomplete row.
//   - sage.guard_grant_requests holds an agent's capability requests until
//     an operator approves or denies them (every G1 grant is L2).
//   - action_queue_principal_status keeps D9's per-agent pending count
//     (agents.approvals.max_pending_per_principal) off a sequential scan.
const ddlGuardGrants = `
CREATE TABLE IF NOT EXISTS sage.guard_grants (
    id               bigserial PRIMARY KEY,
    database_id      uuid NOT NULL,
    principal_id     text NOT NULL CHECK (principal_id ~ '^agp_[a-z2-7]{20}$'),
    lane             text NOT NULL CHECK (lane IN ('direct', 'broker')),
    capability       text NOT NULL CHECK (capability ~ '^[a-z][a-z0-9_]{0,63}$'),
    object_oid       oid NOT NULL,
    object_name      text NOT NULL,
    columns          text[] NOT NULL,
    privileges       text[] NOT NULL,
    grantor          text NOT NULL,
    granted_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    revoked_at       timestamptz,
    state            text NOT NULL DEFAULT 'active'
                     CHECK (state IN ('active', 'revoked', 'revoke_incomplete')),
    grant_action_id  bigint NOT NULL,
    revoke_action_id bigint,
    CHECK (expires_at > granted_at)
);
ALTER TABLE sage.guard_grants ADD COLUMN IF NOT EXISTS object_kind text NOT NULL
    DEFAULT 'relation' CHECK (object_kind IN ('relation', 'schema'));
ALTER TABLE sage.guard_grants ADD COLUMN IF NOT EXISTS schema_oid oid;
ALTER TABLE sage.guard_grants ADD COLUMN IF NOT EXISTS revoke_detail text
    CHECK (length(revoke_detail) <= 4000);
CREATE INDEX IF NOT EXISTS guard_grants_open_idx ON sage.guard_grants (expires_at)
    WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS guard_grants_principal_idx
    ON sage.guard_grants (principal_id, id DESC);
CREATE INDEX IF NOT EXISTS guard_grants_object_idx
    ON sage.guard_grants (principal_id, object_oid) WHERE state <> 'revoked';
CREATE INDEX IF NOT EXISTS guard_grants_incomplete_idx ON sage.guard_grants (id)
    WHERE state = 'revoke_incomplete';
CREATE TABLE IF NOT EXISTS sage.guard_grant_requests (
    id               bigserial PRIMARY KEY,
    database_id      uuid NOT NULL,
    principal_id     text NOT NULL CHECK (principal_id ~ '^agp_[a-z2-7]{20}$'),
    capability       text NOT NULL CHECK (capability ~ '^[a-z][a-z0-9_]{0,63}$'),
    objects          jsonb NOT NULL,
    duration_minutes integer NOT NULL CHECK (duration_minutes > 0),
    reason           text NOT NULL CHECK (length(reason) BETWEEN 1 AND 2000),
    task_id          text,
    status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending',
                     'approved', 'denied', 'expired', 'failed', 'recorded')),
    reason_code      text,
    detail           text CHECK (length(detail) <= 4000),
    requested_at     timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    decided_by       integer,
    decided_at       timestamptz,
    grant_ids        bigint[] NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS guard_grant_requests_principal_idx
    ON sage.guard_grant_requests (principal_id, id DESC);
CREATE INDEX IF NOT EXISTS guard_grant_requests_pending_idx
    ON sage.guard_grant_requests (principal_id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS action_queue_principal_status
    ON sage.action_queue (principal_id, status) WHERE principal_id IS NOT NULL;
`
