package schema

const ddlAgentNativePolicy = `
CREATE TABLE IF NOT EXISTS sage.policy (
    id              bigserial PRIMARY KEY,
    database_id     bigint,
    version         integer NOT NULL,
    scope           text NOT NULL DEFAULT 'database',
    profile         text NOT NULL DEFAULT 'staffed',
    doc             jsonb NOT NULL,
	preview         jsonb NOT NULL DEFAULT '{"changed_pending_outcomes":[]}'::jsonb,
    schema_version  integer NOT NULL DEFAULT 1,
    status          text NOT NULL DEFAULT 'proposed',
    proposed_by     text NOT NULL,
    ratified_by     text,
    proposed_at     timestamptz NOT NULL DEFAULT now(),
    ratified_at     timestamptz,
    activated_at    timestamptz,
    supersedes_id   bigint REFERENCES sage.policy(id),
    CHECK (status IN ('proposed', 'ratified', 'active', 'superseded', 'rejected')),
    CHECK (profile IN ('staffed', 'unattended')),
    UNIQUE (database_id, version)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_policy_active_database_status
    ON sage.policy ((COALESCE(database_id, 0)), scope)
    WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_policy_history
    ON sage.policy (database_id, version DESC);
`
