package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auditchain"
)

// apiauth-01: sage.users gains disabled_at; sage.config_audit.changed_by
// loses its foreign key (the column is sealed in the audit chain, so it is
// never rewritten) and sage.config.updated_by_user_id's key becomes ON
// DELETE SET NULL. Deleting a user who once changed config then succeeds.

const legacyConfigUserFKs = `
DO $legacy$
DECLARE c record;
BEGIN
    FOR c IN SELECT con.conrelid::regclass AS rel, con.conname
               FROM pg_constraint con
               JOIN pg_attribute a ON a.attrelid = con.conrelid
                AND a.attnum = ANY (con.conkey)
              WHERE con.contype = 'f'
                AND ((con.conrelid = 'sage.config_audit'::regclass
                      AND a.attname = 'changed_by')
                  OR (con.conrelid = 'sage.config'::regclass
                      AND a.attname = 'updated_by_user_id'))
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', c.rel, c.conname);
    END LOOP;
END
$legacy$;
-- NOT VALID: earlier tests may leave audit rows naming deleted users.
ALTER TABLE sage.config_audit ADD CONSTRAINT config_audit_changed_by_fkey
    FOREIGN KEY (changed_by) REFERENCES sage.users(id) NOT VALID;
ALTER TABLE sage.config ADD CONSTRAINT config_updated_by_user_id_fkey
    FOREIGN KEY (updated_by_user_id) REFERENCES sage.users(id);
ALTER TABLE sage.users DROP COLUMN IF EXISTS disabled_at;
`

func TestUsersDisabledAndConfigUserFKMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	requireUserFKShape(t, ctx, pool)
	if _, err := pool.Exec(ctx, legacyConfigUserFKs); err != nil {
		t.Fatalf("simulating the pre-apiauth-01 schema: %v", err)
	}
	uid := sessionsMigrationUser(t, ctx, pool, "fk-mig@example.com")
	rowID := insertConfigChange(t, ctx, pool, uid)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM sage.config WHERE key = 'apiauth01.probe'`)
		_, _ = pool.Exec(bg, `DELETE FROM sage.users WHERE id = $1`, uid)
	})

	bootstrapWithRetry(t, ctx, pool)
	requireUserFKShape(t, ctx, pool)
	requireConfigAuditRowVerifies(t, ctx, pool, rowID)
	fkOID := configFKOID(t, ctx, pool)

	if _, err := pool.Exec(ctx, `DELETE FROM sage.users WHERE id = $1`, uid); err != nil {
		t.Fatalf("deleting a user who changed config: %v", err)
	}
	var changedBy *int
	if err := pool.QueryRow(ctx, `SELECT changed_by FROM sage.config_audit
		WHERE id = $1`, rowID).Scan(&changedBy); err != nil {
		t.Fatalf("reading audit row: %v", err)
	}
	if changedBy == nil || *changedBy != uid {
		t.Fatalf("config_audit.changed_by = %v, want %d kept", changedBy, uid)
	}
	var updatedBy *int
	if err := pool.QueryRow(ctx, `SELECT updated_by_user_id FROM sage.config
		WHERE key = 'apiauth01.probe'`).Scan(&updatedBy); err != nil {
		t.Fatalf("reading config row: %v", err)
	}
	if updatedBy != nil {
		t.Fatalf("config.updated_by_user_id = %d, want NULL after delete", *updatedBy)
	}
	requireConfigAuditRowVerifies(t, ctx, pool, rowID)

	bootstrapWithRetry(t, ctx, pool) // second run is a no-op
	requireUserFKShape(t, ctx, pool)
	if got := configFKOID(t, ctx, pool); got != fkOID {
		t.Fatalf("re-run recreated the sage.config FK (oid %d -> %d)", fkOID, got)
	}
}

// A sage.config row naming a user that no longer exists (possible only
// where the key was missing) is cleared so the new key validates.
func TestConfigUserFKMigrationClearsDanglingUpdater(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, legacyConfigUserFKs+`
		ALTER TABLE sage.config DROP CONSTRAINT config_updated_by_user_id_fkey;
		ALTER TABLE sage.config_audit DROP CONSTRAINT config_audit_changed_by_fkey;
		INSERT INTO sage.config (key, value, updated_by_user_id)
		VALUES ('apiauth01.dangling', 'x', 2147483000)`); err != nil {
		t.Fatalf("simulating a dangling updater: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.config WHERE key = 'apiauth01.dangling'`)
	})
	bootstrapWithRetry(t, ctx, pool)
	requireUserFKShape(t, ctx, pool)
	var updatedBy *int
	if err := pool.QueryRow(ctx, `SELECT updated_by_user_id FROM sage.config
		WHERE key = 'apiauth01.dangling'`).Scan(&updatedBy); err != nil {
		t.Fatalf("reading dangling row: %v", err)
	}
	if updatedBy != nil {
		t.Fatalf("dangling updated_by_user_id = %d, want NULL", *updatedBy)
	}
}

func insertConfigChange(t *testing.T, ctx context.Context, pool *pgxpool.Pool, uid int) int64 {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.config (key, value, updated_by_user_id)
		VALUES ('apiauth01.probe', 'on', $1)`, uid); err != nil {
		t.Fatalf("inserting config row: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.config_audit
		(key, old_value, new_value, changed_by) VALUES ('apiauth01.probe', NULL, 'on', $1)
		RETURNING id`, uid).Scan(&id); err != nil {
		t.Fatalf("inserting config_audit row: %v", err)
	}
	return id
}

func requireUserFKShape(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var auditFKs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'sage.config_audit'::regclass AND contype = 'f'`).
		Scan(&auditFKs); err != nil {
		t.Fatalf("counting config_audit FKs: %v", err)
	}
	if auditFKs != 0 {
		t.Fatalf("sage.config_audit has %d foreign keys, want 0", auditFKs)
	}
	var delType string
	var validated bool
	if err := pool.QueryRow(ctx, `SELECT con.confdeltype::text, con.convalidated
		FROM pg_constraint con
		JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = ANY (con.conkey)
		WHERE con.conrelid = 'sage.config'::regclass AND con.contype = 'f'
		  AND a.attname = 'updated_by_user_id'`).Scan(&delType, &validated); err != nil {
		t.Fatalf("reading sage.config updater FK: %v", err)
	}
	if delType != "n" || !validated {
		t.Fatalf("sage.config FK confdeltype=%q validated=%v, want n and true",
			delType, validated)
	}
	var typ string
	var notNull bool
	if err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod), attnotnull
		FROM pg_attribute WHERE attrelid = 'sage.users'::regclass
		  AND attname = 'disabled_at' AND NOT attisdropped`).Scan(&typ, &notNull); err != nil {
		t.Fatalf("reading sage.users.disabled_at: %v", err)
	}
	if typ != "timestamp with time zone" || notNull {
		t.Fatalf("disabled_at is %s (not null %v), want nullable timestamptz", typ, notNull)
	}
}

func configFKOID(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uint32 {
	t.Helper()
	var oid uint32
	if err := pool.QueryRow(ctx, `SELECT con.oid FROM pg_constraint con
		JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = ANY (con.conkey)
		WHERE con.conrelid = 'sage.config'::regclass AND con.contype = 'f'
		  AND a.attname = 'updated_by_user_id'`).Scan(&oid); err != nil {
		t.Fatalf("reading FK oid: %v", err)
	}
	return oid
}

func requireConfigAuditRowVerifies(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, rowID int64,
) {
	t.Helper()
	links, err := auditchain.LinksForRow(ctx, pool, "config_audit", rowID)
	if err != nil || len(links) == 0 {
		t.Fatalf("links of config_audit row %d = (%d, %v)", rowID, len(links), err)
	}
	for _, l := range links {
		if l.Op != "I" {
			t.Fatalf("config_audit row %d has a %q link, want only its insert", rowID, l.Op)
		}
	}
	rep, err := auditchain.Verify(ctx, pool, auditchain.ConfigAudit,
		auditchain.Window{FromSeq: links[0].Seq, ToSeq: links[0].Seq})
	if err != nil {
		t.Fatalf("verifying config_audit: %v", err)
	}
	if !rep.OK() || rep.RowsChecked < 1 {
		t.Fatalf("config_audit verify = %+v, want OK with the row checked", rep)
	}
}
