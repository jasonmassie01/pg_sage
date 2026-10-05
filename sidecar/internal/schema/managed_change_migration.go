package schema

// Managed clouds (roadmap phase 3): sage.managed_change_proposals holds the
// typed parameter-group / database-flag changes pg_sage proposes on RDS,
// Aurora and Cloud SQL. pg_sage never applies them: an operator approves
// (and runs the command) or rejects; pg_sage marks a proposal applied when
// PostgreSQL runs the new value, superseded when its reason is gone or a
// newer value replaces it. At most one open proposal per fingerprint.
// Idempotent.
const ddlManagedChangeProposals = `
CREATE TABLE IF NOT EXISTS sage.managed_change_proposals (
    id              bigserial PRIMARY KEY,
    fingerprint     text NOT NULL CHECK (length(fingerprint) BETWEEN 1 AND 64),
    provider        text NOT NULL CHECK (provider IN ('rds', 'aurora', 'cloud-sql')),
    mechanism       text NOT NULL,
    target          text NOT NULL,
    parameter       text NOT NULL,
    value           text NOT NULL,
    reboot_required boolean NOT NULL,
    proposal        jsonb NOT NULL,
    finding_id      bigint,
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN
        ('pending', 'approved', 'rejected', 'applied', 'superseded')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    decided_by      integer,
    decided_at      timestamptz,
    decision_note   text NOT NULL DEFAULT '',
    applied_at      timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS managed_change_open_fingerprint_idx
    ON sage.managed_change_proposals (fingerprint)
    WHERE status IN ('pending', 'approved');
CREATE INDEX IF NOT EXISTS managed_change_status_idx
    ON sage.managed_change_proposals (status, created_at DESC);
CREATE INDEX IF NOT EXISTS managed_change_parameter_idx
    ON sage.managed_change_proposals (provider, target, parameter)
    WHERE status IN ('pending', 'approved');
`
