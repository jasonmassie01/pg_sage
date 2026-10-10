package schema

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// ddlGuardPGAuditEvents keeps the pgaudit records pg_sage correlated to its
// own actions or to an agent principal (E2, §6.17). Only correlated
// records are kept, so its size follows pg_sage and agent activity, not the
// database's whole audit log.
const ddlGuardPGAuditEvents = `
CREATE TABLE IF NOT EXISTS sage.guard_pgaudit_events (
    id              bigserial   PRIMARY KEY,
    database_name   text        NOT NULL,
    logged_at       timestamptz NOT NULL,
    session_id      text        NOT NULL DEFAULT '',
    pid             integer     NOT NULL DEFAULT 0,
    db_user         text        NOT NULL DEFAULT '',
    application     text        NOT NULL DEFAULT '',
    principal_id    text,
    action_id       bigint,
    audit_type      text        NOT NULL,
    statement_id    bigint      NOT NULL DEFAULT 0,
    substatement_id bigint      NOT NULL DEFAULT 0,
    class           text        NOT NULL DEFAULT '',
    command         text        NOT NULL DEFAULT '',
    object_type     text        NOT NULL DEFAULT '',
    object_name     text        NOT NULL DEFAULT '',
    statement       text        NOT NULL DEFAULT '',
    correlated_by   text        NOT NULL,
    captured_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS guard_pgaudit_events_action
    ON sage.guard_pgaudit_events (action_id) WHERE action_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS guard_pgaudit_events_logged
    ON sage.guard_pgaudit_events (logged_at);
`

// ddlSIEMCursor keeps each SIEM sink's export position per source database
// and audit chain (E2). Rows only move forward.
const ddlSIEMCursor = `
CREATE TABLE IF NOT EXISTS sage.siem_cursor (
    sink       text        NOT NULL,
    source     text        NOT NULL,
    chain      text        NOT NULL,
    seq        bigint      NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (sink, source, chain)
);
`

// ddlGuardIdentityBindings maps an external identity (issuer, subject) to a
// principal (E2, §7). It references sage.guard_principals, which the G1
// core migration creates, so it is created once that table exists.
const ddlGuardIdentityBindings = `
DO $do$
BEGIN
    IF to_regclass('sage.guard_principals') IS NULL THEN
        RETURN;
    END IF;
    CREATE TABLE IF NOT EXISTS sage.guard_identity_bindings (
        issuer       text NOT NULL,
        subject      text NOT NULL,
        principal_id text NOT NULL REFERENCES sage.guard_principals(id) ON DELETE CASCADE,
        created_by   text NOT NULL,
        created_at   timestamptz NOT NULL DEFAULT now(),
        PRIMARY KEY (issuer, subject)
    );
    CREATE INDEX IF NOT EXISTS guard_identity_bindings_principal
        ON sage.guard_identity_bindings (principal_id);
END $do$;
`

// migrateAuditChain installs the hash chains over the audit tables (E2).
// It is Bootstrap's last step, so every table it chains exists (the config
// migration creates sage.config_audit after the migration list runs).
func migrateAuditChain(ctx context.Context, db bootstrapDB) error {
	qctx, cancel := context.WithTimeout(ctx, migrationBatchTimeout)
	defer cancel()
	if _, err := db.Exec(qctx, auditchain.MigrationSQL()); err != nil {
		return fmt.Errorf("audit chain migration: %w", err)
	}
	return nil
}
