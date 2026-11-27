package schema

// ddlGuardPrincipalChanges (agentgov-06): sage.guard_principal_changes on
// the control database holds a widening principal change (a higher
// ceiling, a broader profile) that waits for a second admin. patch is the
// requested {sponsor_user_id, profile, env_ceiling} subset; at most one
// change is pending per principal (partial unique index). Idempotent: the
// table and index are created only when missing, so a second boot takes
// no lock; lock_timeout bounds the first.
const ddlGuardPrincipalChanges = `
SET LOCAL lock_timeout = '5s';
DO $do$
BEGIN
    IF to_regclass('sage.guard_principals') IS NULL THEN
        RETURN;
    END IF;
    IF to_regclass('sage.guard_principal_changes') IS NULL THEN
        CREATE TABLE sage.guard_principal_changes (
            id                bigserial PRIMARY KEY,
            principal_id      text NOT NULL
                              REFERENCES sage.guard_principals(id) ON DELETE CASCADE,
            patch             jsonb NOT NULL CHECK (jsonb_typeof(patch) = 'object'),
            requested_by      text NOT NULL CHECK (length(requested_by) BETWEEN 1 AND 200),
            requested_by_user integer NOT NULL CHECK (requested_by_user > 0),
            reason            text NOT NULL DEFAULT '' CHECK (length(reason) <= 2000),
            requested_at      timestamptz NOT NULL DEFAULT now(),
            expires_at        timestamptz NOT NULL,
            status            text NOT NULL DEFAULT 'pending'
                              CHECK (status IN ('pending', 'applied', 'expired',
                                                'superseded')),
            decided_by        text CHECK (length(decided_by) <= 200),
            decided_by_user   integer,
            decided_at        timestamptz
        );
    END IF;
    IF to_regclass('sage.guard_principal_changes_pending') IS NULL THEN
        CREATE UNIQUE INDEX guard_principal_changes_pending
            ON sage.guard_principal_changes (principal_id) WHERE status = 'pending';
    END IF;
END $do$;
`
