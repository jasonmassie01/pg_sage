package schema

// ddlPolicySets (fleetpol-01a) adds named policy sets. A set's document is
// an ordinary sage.policy row (database_id NULL, set_name = the set,
// scope 'policy_set:<name>' so the existing one-active-row-per-scope index
// holds per set), so versions and propose-then-ratify are reused. A
// database's own active document is an exception against the set it
// matches; sage.policy_exceptions says who set it, why and until when, and
// sage.policy_set_events keeps the audit trail. Seeds: default (every
// database, priority 0), prod (tag tier=prod) and agent-created (creator
// agent:*). Every per-database document an earlier release wrote becomes an
// exception against default, its document untouched.
const ddlPolicySets = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE sage.policy ADD COLUMN IF NOT EXISTS set_name text;
CREATE INDEX IF NOT EXISTS idx_policy_set_history
    ON sage.policy (set_name, version DESC) WHERE set_name IS NOT NULL;
CREATE TABLE IF NOT EXISTS sage.policy_sets (
    name         text PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9_-]{0,62}$'),
    priority     integer NOT NULL CHECK (priority BETWEEN 0 AND 1000),
    selectors    jsonb NOT NULL DEFAULT '[]'::jsonb
        CHECK (jsonb_typeof(selectors) = 'array'),
    requirements jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(requirements) = 'object'),
    created_by   text NOT NULL,
    updated_by   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS sage.policy_exceptions (
    database_id bigint PRIMARY KEY,
    set_name    text NOT NULL,
    diff        jsonb NOT NULL DEFAULT '[]'::jsonb,
    set_by      text NOT NULL,
    reason      text NOT NULL CHECK (length(reason) BETWEEN 1 AND 2000),
    set_at      timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz
);
CREATE TABLE IF NOT EXISTS sage.policy_set_events (
    id          bigserial PRIMARY KEY,
    at          timestamptz NOT NULL DEFAULT now(),
    event       text NOT NULL
        CHECK (event IN ('set_saved', 'exception_set', 'exception_expired')),
    set_name    text,
    database_id bigint,
    actor       text NOT NULL,
    detail      jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS idx_policy_set_events_at ON sage.policy_set_events (at DESC);
INSERT INTO sage.policy_sets (name, priority, selectors, created_by) VALUES
    ('default', 0, '[{"all": true}]', 'migration'),
    ('prod', 10, '[{"tags": {"tier": "prod"}}]', 'migration'),
    ('agent-created', 20, '[{"creator": "agent:*"}]', 'migration')
ON CONFLICT (name) DO NOTHING;
INSERT INTO sage.policy_exceptions (database_id, set_name, set_by, reason, set_at)
SELECT p.database_id, 'default', COALESCE(p.ratified_by, p.proposed_by),
       'per-database policy from before policy sets',
       COALESCE(p.activated_at, p.ratified_at, p.proposed_at)
FROM sage.policy p
WHERE p.database_id IS NOT NULL AND p.status = 'active'
ON CONFLICT (database_id) DO NOTHING;
`
