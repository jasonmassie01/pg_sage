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
// and audit chain (E2). Rows only move forward, except by an admin's
// re-anchor (ddlSIEMCursorAnchor adds the columns it uses).
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
// principal (E2, §7), and records which principals file manages a
// principal. Both reference sage.guard_principals, created by the G1 core
// migration that runs before this one.
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
    -- The declarative principals file that manages each principal (E2, PC-1).
    CREATE TABLE IF NOT EXISTS sage.guard_principal_files (
        principal_id text PRIMARY KEY
                     REFERENCES sage.guard_principals(id) ON DELETE CASCADE,
        file_name    text NOT NULL CHECK (file_name ~ '^[a-z][a-z0-9-]{1,62}$'),
        applied_by   text NOT NULL,
        applied_at   timestamptz NOT NULL DEFAULT now()
    );
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

// ddlSIEMCursorAnchor adds what lets SIEM export see a chain going back
// (audit-01): the hash of the last exported link (NULL on cursors saved
// before it, checked by existence then filled) and how far a re-anchor
// sent the cursor back. The guard trigger drops the hash when an update
// moves seq without writing one, which is what a sidecar from before this
// column does during a rolling upgrade; a stale hash would otherwise stop a
// healthy chain as regressed. The catalog checks keep a second bootstrap
// from taking the table lock at all.
const ddlSIEMCursorAnchor = `
CREATE OR REPLACE FUNCTION sage.siem_cursor_hash_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    IF NEW.seq IS DISTINCT FROM OLD.seq AND NEW.hash IS NOT DISTINCT FROM OLD.hash THEN
        NEW.hash := NULL;
    END IF;
    RETURN NEW;
END $fn$;
DO $do$
BEGIN
    IF to_regclass('sage.siem_cursor') IS NULL THEN
        RETURN;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_attribute
                   WHERE attrelid = 'sage.siem_cursor'::regclass
                   AND attname = 'fork_to' AND NOT attisdropped) THEN
        ALTER TABLE sage.siem_cursor ADD COLUMN IF NOT EXISTS hash text,
            ADD COLUMN IF NOT EXISTS fork_to bigint;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger
                   WHERE tgrelid = 'sage.siem_cursor'::regclass
                   AND tgname = 'siem_cursor_hash_guard') THEN
        CREATE TRIGGER siem_cursor_hash_guard BEFORE UPDATE ON sage.siem_cursor
            FOR EACH ROW EXECUTE FUNCTION sage.siem_cursor_hash_guard();
    END IF;
END $do$;
`

// siemCursorLockTimeoutSQL bounds the wait for the cursor table's lock:
// the exporter holds it only for one short upsert.
const siemCursorLockTimeoutSQL = "SET LOCAL lock_timeout = '5s'"

// migrateSIEMCursorAnchor runs ddlSIEMCursorAnchor under lock_timeout.
func migrateSIEMCursorAnchor(ctx context.Context, db bootstrapDB) error {
	qctx, cancel := context.WithTimeout(ctx, migrationBatchTimeout)
	defer cancel()
	tx, err := db.Begin(qctx)
	if err != nil {
		return fmt.Errorf("siem cursor migration: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(qctx) }()
	if _, err := tx.Exec(qctx, siemCursorLockTimeoutSQL); err != nil {
		return fmt.Errorf("siem cursor migration: set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(qctx, ddlSIEMCursorAnchor); err != nil {
		return fmt.Errorf("siem cursor migration: %w", err)
	}
	if err := tx.Commit(qctx); err != nil {
		return fmt.Errorf("siem cursor migration: commit: %w", err)
	}
	return nil
}
