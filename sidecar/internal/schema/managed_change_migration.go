package schema

// Managed clouds (roadmap phase 3): sage.managed_change_proposals holds the
// typed parameter-group / database-flag changes pg_sage proposes on RDS,
// Aurora and Cloud SQL. pg_sage never applies them: an operator approves
// (and runs the command) or rejects; pg_sage marks a proposal applied when
// PostgreSQL runs the new value, superseded when its reason is gone or a
// newer value replaces it. At most one open proposal per fingerprint
// (provider, parameter and value; see ddlManagedChangeFingerprintV2).
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

// ddlManagedChangeFingerprintV2 (clouds-02) moves managed change
// fingerprints to v2: "v2:" plus the first 16 hex digits of
// sha256(provider|parameter|value), as managedparam computes them. The
// legacy fingerprint also hashed the target, so a transient
// target-resolution failure (placeholders) forked a second open proposal
// for the same change and evaded the rejection cooldown. Duplicate open
// rows of one change are superseded first, keeping an approved row over a
// pending one and then the newest, so the open-fingerprint unique index
// holds; then every legacy fingerprint, open or closed, is recomputed in
// the column and in the stored proposal. sha256() is built in (PG11+), no
// pgcrypto. Idempotent: a second run finds no duplicates and no legacy
// fingerprint.
const ddlManagedChangeFingerprintV2 = `
SET LOCAL lock_timeout = '5s';
WITH ranked AS (
    SELECT id, row_number() OVER (PARTITION BY provider, parameter, value
        ORDER BY (status = 'approved') DESC, updated_at DESC, id DESC) AS rn
    FROM sage.managed_change_proposals
    WHERE status IN ('pending', 'approved')
)
UPDATE sage.managed_change_proposals m
SET status = 'superseded', updated_at = now()
FROM ranked r
WHERE m.id = r.id AND r.rn > 1;
UPDATE sage.managed_change_proposals m
SET fingerprint = v2.fp,
    proposal = CASE WHEN jsonb_typeof(m.proposal) = 'object'
                    THEN jsonb_set(m.proposal, '{fingerprint}', to_jsonb(v2.fp))
                    ELSE m.proposal END
FROM (SELECT id, 'v2:' || left(encode(sha256(convert_to(
             provider || '|' || parameter || '|' || value, 'UTF8')), 'hex'), 16) AS fp
      FROM sage.managed_change_proposals
      WHERE fingerprint NOT LIKE 'v2:%') v2
WHERE m.id = v2.id;
`
