package schema

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const ddlConfigAudit = `
CREATE TABLE IF NOT EXISTS sage.config_audit (
    id          SERIAL PRIMARY KEY,
    key         TEXT NOT NULL,
    old_value   TEXT,
    new_value   TEXT NOT NULL,
    database_id INT,
    changed_by  INT,
    changed_at  TIMESTAMPTZ DEFAULT now()
);
-- changed_by_actor names non-user actors and fleet-wide operators whose
-- user row lives in another database (emergency stop attribution, D8).
ALTER TABLE sage.config_audit
    ADD COLUMN IF NOT EXISTS changed_by_actor TEXT;
CREATE INDEX IF NOT EXISTS idx_config_audit_time
    ON sage.config_audit (changed_at DESC);
CREATE INDEX IF NOT EXISTS idx_config_audit_db
    ON sage.config_audit (database_id, changed_at DESC);
`

// MigrateConfigSchema extends sage.config with database_id and
// updated_by_user_id columns (if missing), drops the old text
// unique constraint on key, adds a composite unique on
// (key, database_id), and creates sage.config_audit.
func MigrateConfigSchema(
	ctx context.Context, pool *pgxpool.Pool,
) error {
	return migrateConfigSchema(ctx, pool)
}

func migrateConfigSchema(ctx context.Context, db bootstrapDB) error {
	if err := addConfigColumns(ctx, db); err != nil {
		return fmt.Errorf("config columns: %w", err)
	}
	if err := ensureConfigAudit(ctx, db); err != nil {
		return fmt.Errorf("config_audit: %w", err)
	}
	if err := migrateConfigUserKeys(ctx, db); err != nil {
		return fmt.Errorf("config user keys: %w", err)
	}
	return nil
}

// migrateConfigUserKeys lets a user who changed config be deleted
// (apiauth-01): the audit trail keeps the id without a key, and
// sage.config forgets its updater. Add and validate run in separate
// transactions so validation does not hold the add's lock.
func migrateConfigUserKeys(ctx context.Context, db bootstrapDB) error {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := db.Exec(qctx, ddlConfigUserKeys); err != nil {
		return fmt.Errorf("reshaping config user foreign keys: %w", err)
	}
	if _, err := db.Exec(qctx, ddlConfigUserKeyValidate); err != nil {
		return fmt.Errorf("validating sage.config updater foreign key: %w", err)
	}
	return nil
}

func addConfigColumns(
	ctx context.Context, db bootstrapDB,
) error {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Add database_id column if missing.
	_, err := db.Exec(qctx, `/* pg_sage */
		DO $$ BEGIN
			ALTER TABLE sage.config
				ADD COLUMN database_id INT;
		EXCEPTION
			WHEN duplicate_column THEN NULL;
		END $$;
	`)
	if err != nil {
		return fmt.Errorf("adding database_id: %w", err)
	}

	// Add updated_by_user_id column if missing.
	_, err = db.Exec(qctx, `/* pg_sage */
		DO $$ BEGIN
			ALTER TABLE sage.config
				ADD COLUMN updated_by_user_id INT
				REFERENCES sage.users(id) ON DELETE SET NULL;
		EXCEPTION
			WHEN duplicate_column THEN NULL;
		END $$;
	`)
	if err != nil {
		return fmt.Errorf("adding updated_by_user_id: %w", err)
	}

	return addConfigCompositeIndex(qctx, db)
}

func addConfigCompositeIndex(
	ctx context.Context, db bootstrapDB,
) error {
	// Drop the old single-column PK on (key) so per-database
	// overrides can coexist with global config rows.
	_, err := db.Exec(ctx, `/* pg_sage */
		DO $$ BEGIN
			ALTER TABLE sage.config
				DROP CONSTRAINT IF EXISTS config_pkey;
		EXCEPTION
			WHEN others THEN NULL;
		END $$;
	`)
	if err != nil {
		return fmt.Errorf("dropping old PK: %w", err)
	}

	_, err = db.Exec(ctx, `/* pg_sage */
		DO $$ BEGIN
			CREATE UNIQUE INDEX IF NOT EXISTS
				idx_config_key_db
				ON sage.config (key, COALESCE(database_id, 0));
		EXCEPTION
			WHEN others THEN NULL;
		END $$;
	`)
	if err != nil {
		return fmt.Errorf("creating composite index: %w", err)
	}
	return nil
}

func ensureConfigAudit(
	ctx context.Context, db bootstrapDB,
) error {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err := db.Exec(qctx, ddlConfigAudit)
	if err != nil {
		return fmt.Errorf("creating config_audit: %w", err)
	}
	return nil
}

// ddlConfigUserKeys reshapes the two config foreign keys to sage.users so
// deleting a user who once changed config succeeds (apiauth-01).
// sage.config_audit.changed_by is sealed in the audit chain, so it keeps a
// plain id with no key (as sage.auth_audit does): an ON DELETE action
// would rewrite a sealed column. Keys are found by column, not by name.
// sage.config is not chained: its updater key becomes ON DELETE SET NULL,
// added NOT VALID here and validated by ddlConfigUserKeyValidate in its own
// transaction. Each step checks the catalog first, so a re-run takes no
// lock.
const ddlConfigUserKeys = `
SET LOCAL lock_timeout = '5s';
DO $config_user_keys$
DECLARE
    c record;
BEGIN
    FOR c IN
        SELECT con.conrelid::regclass AS rel, con.conname
          FROM pg_constraint con
          JOIN pg_attribute a ON a.attrelid = con.conrelid
           AND a.attnum = ANY (con.conkey)
         WHERE con.contype = 'f'
           AND ((con.conrelid = 'sage.config_audit'::regclass
                 AND a.attname = 'changed_by')
             OR (con.conrelid = 'sage.config'::regclass
                 AND a.attname = 'updated_by_user_id'
                 AND con.confdeltype <> 'n'))
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', c.rel, c.conname);
    END LOOP;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint con
          JOIN pg_attribute a ON a.attrelid = con.conrelid
           AND a.attnum = ANY (con.conkey)
         WHERE con.conrelid = 'sage.config'::regclass AND con.contype = 'f'
           AND a.attname = 'updated_by_user_id'
    ) THEN
        UPDATE sage.config cfg SET updated_by_user_id = NULL
         WHERE cfg.updated_by_user_id IS NOT NULL
           AND NOT EXISTS (SELECT 1 FROM sage.users u
                            WHERE u.id = cfg.updated_by_user_id);
        ALTER TABLE sage.config ADD CONSTRAINT config_updated_by_user_id_fkey
            FOREIGN KEY (updated_by_user_id) REFERENCES sage.users(id)
            ON DELETE SET NULL NOT VALID;
    END IF;
END
$config_user_keys$;
`

// ddlConfigUserKeyValidate validates the key ddlConfigUserKeys added NOT
// VALID; it reads sage.config without blocking its writers.
const ddlConfigUserKeyValidate = `
SET LOCAL lock_timeout = '5s';
DO $config_user_key_validate$
DECLARE
    c record;
BEGIN
    FOR c IN
        SELECT con.conname
          FROM pg_constraint con
          JOIN pg_attribute a ON a.attrelid = con.conrelid
           AND a.attnum = ANY (con.conkey)
         WHERE con.conrelid = 'sage.config'::regclass AND con.contype = 'f'
           AND a.attname = 'updated_by_user_id' AND NOT con.convalidated
    LOOP
        EXECUTE format('ALTER TABLE sage.config VALIDATE CONSTRAINT %I', c.conname);
    END LOOP;
END
$config_user_key_validate$;
`
