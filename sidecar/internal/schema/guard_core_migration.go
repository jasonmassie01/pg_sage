package schema

// Agent governance G1 core (AGENTDB-SPEC §6.4, §6.6, §7). All idempotent.
//
// Control database (the tables exist in every bootstrapped database; only
// the control database's rows are used):
//   - sage.guard_principals: one row per agent identity, with its
//     accountable sponsor (a sage.users row; none means L0), profile,
//     environment ceiling and status (active, frozen, retired).
//   - sage.mcp_tokens.principal_id binds an agent token to its principal.
//   - sage.guard_cluster_roles: the two roles of a principal on one
//     cluster, and the broker login's credential sealed with the
//     encryption key (associated data principal_id||cluster_key||role).
//   - sage.guard_taint: no time expiry; cleared only per
//     agents.taint.clear_on (§6.10).
//
// Every monitored database:
//   - sage.guard_public_baseline: what PUBLIC could do when the preflight
//     ran (§6.6 P4). An agent role is a member of PUBLIC, so its effective
//     privileges are its grants plus this baseline (G1-02).
//   - provenance columns on action_log, action_queue and decision.
const ddlGuardCore = `
CREATE TABLE IF NOT EXISTS sage.guard_principals (
    id              text PRIMARY KEY CHECK (id ~ '^agp_[a-z2-7]{20}$'),
    name            text NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9-]{1,62}$'),
    sponsor_user_id integer REFERENCES sage.users(id) ON DELETE SET NULL,
    tenant          text NOT NULL DEFAULT '' CHECK (length(tenant) <= 200),
    profile         text NOT NULL CHECK (length(profile) BETWEEN 1 AND 100),
    env_ceiling     text NOT NULL DEFAULT 'dev'
                    CHECK (env_ceiling IN ('branch', 'dev', 'stage', 'prod')),
    status          text NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'frozen', 'retired')),
    frozen_reason   text NOT NULL DEFAULT '' CHECK (length(frozen_reason) <= 2000),
    created_by      text NOT NULL CHECK (length(created_by) BETWEEN 1 AND 200),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS guard_principals_sponsor
    ON sage.guard_principals (sponsor_user_id) WHERE sponsor_user_id IS NOT NULL;
ALTER TABLE sage.mcp_tokens ADD COLUMN IF NOT EXISTS principal_id text
    REFERENCES sage.guard_principals(id);
CREATE INDEX IF NOT EXISTS mcp_tokens_principal
    ON sage.mcp_tokens (principal_id) WHERE principal_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS sage.guard_cluster_roles (
    principal_id     text NOT NULL REFERENCES sage.guard_principals(id),
    cluster_key      text NOT NULL CHECK (length(cluster_key) BETWEEN 1 AND 500),
    login_role       text NOT NULL CHECK (login_role ~ '^sage_agent_[a-z2-7]{10}$'),
    broker_role      text NOT NULL CHECK (broker_role ~ '^sage_agentb_[a-z2-7]{10}$'),
    broker_secret_ct bytea NOT NULL,
    key_id           text NOT NULL,
    valid_until      timestamptz,
    prior_attrs      jsonb,
    status           text NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active', 'killed', 'retired')),
    rotated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, cluster_key),
    UNIQUE (cluster_key, login_role),
    UNIQUE (cluster_key, broker_role)
);
CREATE INDEX IF NOT EXISTS guard_cluster_roles_login
    ON sage.guard_cluster_roles (login_role);
CREATE INDEX IF NOT EXISTS guard_cluster_roles_broker
    ON sage.guard_cluster_roles (broker_role);
CREATE TABLE IF NOT EXISTS sage.guard_taint (
    principal_id text NOT NULL REFERENCES sage.guard_principals(id),
    task_id      text NOT NULL DEFAULT '*',
    source       text NOT NULL CHECK (length(source) BETWEEN 1 AND 2000),
    tainted_at   timestamptz NOT NULL DEFAULT now(),
    cleared_at   timestamptz,
    cleared_by   text,
    PRIMARY KEY (principal_id, task_id, source)
);
CREATE TABLE IF NOT EXISTS sage.guard_public_baseline (
    object_kind text NOT NULL CHECK (object_kind IN ('database', 'schema', 'relation',
                    'column', 'function', 'default_acl')),
    object_oid  oid NOT NULL,
    attnum      smallint NOT NULL DEFAULT 0,
    object_name text NOT NULL,
    privilege   text NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (object_kind, object_oid, attnum, privilege)
);
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS principal_id   text;
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS on_behalf_of   text;
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS task_id        text;
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS approval_id    bigint;
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS envelope_id    text;
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS artifact_hash  text;
ALTER TABLE sage.action_log   ADD COLUMN IF NOT EXISTS policy_version integer;
ALTER TABLE sage.action_queue ADD COLUMN IF NOT EXISTS principal_id   text;
ALTER TABLE sage.action_queue ADD COLUMN IF NOT EXISTS artifact_hash  text;
ALTER TABLE sage.decision     ADD COLUMN IF NOT EXISTS principal_id   text;
ALTER TABLE sage.decision     ADD COLUMN IF NOT EXISTS task_id        text;
ALTER TABLE sage.decision     ADD COLUMN IF NOT EXISTS artifact_hash  text;
CREATE INDEX IF NOT EXISTS idx_action_log_principal
    ON sage.action_log (principal_id, executed_at DESC) WHERE principal_id IS NOT NULL;
/* pg_sage guard_core v1 */
DO $guard_core$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'action_queue_proposed_via_v2'
                   AND conrelid = 'sage.action_queue'::regclass) THEN
        ALTER TABLE sage.action_queue ADD CONSTRAINT action_queue_proposed_via_v2
            CHECK ((proposed_via IS NULL OR proposed_via IN ('ask_sage', 'agent'))
                   AND (proposed_by IS NULL OR length(proposed_by) BETWEEN 1 AND 200))
            NOT VALID;
    END IF;
    ALTER TABLE sage.action_queue DROP CONSTRAINT IF EXISTS action_queue_proposed_via_check;
END
$guard_core$;
`

// ddlGuardCoreValidate validates the new proposed_via constraint in its own
// transaction, after ddlGuardCore committed it NOT VALID (§7).
const ddlGuardCoreValidate = `
/* pg_sage guard_core v1 */
DO $guard_core_validate$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'action_queue_proposed_via_v2'
               AND conrelid = 'sage.action_queue'::regclass AND NOT convalidated) THEN
        ALTER TABLE sage.action_queue VALIDATE CONSTRAINT action_queue_proposed_via_v2;
    END IF;
END
$guard_core_validate$;
`
