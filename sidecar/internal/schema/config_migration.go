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
//
// Its steps consult the migration ledger as Bootstrap's do, so a call
// after Bootstrap (the meta database makes one) issues no DDL (config-04b).
// It never seals the catalog fingerprint: only Bootstrap, which checks
// every step, may.
func MigrateConfigSchema(
	ctx context.Context, pool *pgxpool.Pool,
) error {
	if ledgerFrom(ctx) == nil {
		ctx = withMigrationLedger(ctx, &migrationLedger{})
	}
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

// The config migrations' own budgets (ci-15: a timeout names them).
var (
	configMigrationBudget = budgetExpired{what: "config migration", budget: 15 * time.Second}
	configAuditBudget     = budgetExpired{what: "config audit migration", budget: 10 * time.Second}
)

// migrateConfigUserKeys lets a user who changed config be deleted
// (apiauth-01): the audit trail keeps the id without a key, and
// sage.config forgets its updater. Add and validate run in separate
// transactions so validation does not hold the add's lock.
func migrateConfigUserKeys(ctx context.Context, db bootstrapDB) error {
	qctx, cancel := withBudget(ctx, configMigrationBudget)
	defer cancel()
	if _, err := runMigrationStep(qctx, db, migrationStep{
		name: "ddlConfigUserKeys", sql: ddlConfigUserKeys,
	}); err != nil {
		return fmt.Errorf("reshaping config user foreign keys: %w", err)
	}
	if _, err := runMigrationStep(qctx, db, migrationStep{
		name: "ddlConfigUserKeyValidate", sql: ddlConfigUserKeyValidate,
	}); err != nil {
		return fmt.Errorf("validating sage.config updater foreign key: %w", err)
	}
	return nil
}

// ddlConfigColumns adds the per-database and updater columns to sage.config.
// The precheck leaves out a column that exists, so a second boot takes no
// lock on sage.config.
const ddlConfigColumns = `
ALTER TABLE sage.config ADD COLUMN IF NOT EXISTS database_id INT;
ALTER TABLE sage.config ADD COLUMN IF NOT EXISTS updated_by_user_id INT
    REFERENCES sage.users(id) ON DELETE SET NULL;
`

// configKeyIndex is the unique index every ON CONFLICT (key,
// COALESCE(database_id, 0)) on sage.config needs (config CAS, emergency stop).
const configKeyIndex = "idx_config_key_db"

const ddlConfigKeyIndex = `CREATE UNIQUE INDEX IF NOT EXISTS idx_config_key_db
    ON sage.config (key, COALESCE(database_id, 0))`

// configKeyIndexStateSQL reads whether sage.config_pkey exists and whether
// idx_config_key_db exists, is valid and is unique.
const configKeyIndexStateSQL = `/* pg_sage config key index v1 */
SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint
                WHERE conrelid = 'sage.config'::regclass AND conname = 'config_pkey'),
       i.indexrelid IS NOT NULL, COALESCE(i.indisvalid, false),
       COALESCE(i.indisunique, false)
  FROM (SELECT 1) one
  LEFT JOIN pg_catalog.pg_index i
    ON i.indexrelid = pg_catalog.to_regclass('sage.idx_config_key_db')`

func addConfigColumns(
	ctx context.Context, db bootstrapDB,
) error {
	qctx, cancel := withBudget(ctx, configMigrationBudget)
	defer cancel()
	if _, err := runMigrationStep(qctx, db, migrationStep{
		name: "ddlConfigColumns", sql: ddlConfigColumns,
	}); err != nil {
		return err
	}
	return addConfigCompositeIndex(qctx, db)
}

// configKeyIndexState is what configKeyIndexStateSQL reads.
type configKeyIndexState struct {
	hasPK, exists, valid, unique bool
}

func readConfigKeyIndexState(
	ctx context.Context, db bootstrapDB,
) (configKeyIndexState, error) {
	var st configKeyIndexState
	err := db.QueryRow(ctx, configKeyIndexStateSQL).
		Scan(&st.hasPK, &st.exists, &st.valid, &st.unique)
	if err != nil {
		return st, fmt.Errorf("reading sage.config keys from the catalog: %w", err)
	}
	return st, nil
}

// addConfigCompositeIndex creates idx_config_key_db when it is missing,
// fails naming it unless it is then present, valid and unique (G5-B24:
// errors were swallowed, and a missing index broke every config upsert),
// and only then drops the old single-column primary key on (key), so
// per-database overrides can coexist with global rows and sage.config is
// never left without a unique key.
func addConfigCompositeIndex(
	ctx context.Context, db bootstrapDB,
) error {
	st, err := readConfigKeyIndexState(ctx, db)
	if err != nil {
		return err
	}
	if !st.exists {
		if _, err := db.Exec(ctx, ddlConfigKeyIndex); err != nil {
			return fmt.Errorf("creating unique index sage.%s on sage.config "+
				"(key, COALESCE(database_id, 0)): %w; remove duplicate (key, database_id) "+
				"rows from sage.config and restart", configKeyIndex, err)
		}
		if st, err = readConfigKeyIndexState(ctx, db); err != nil {
			return err
		}
	}
	if !st.exists || !st.valid || !st.unique {
		return fmt.Errorf("sage.%s is not a valid unique index on sage.config "+
			"(exists %t, valid %t, unique %t): drop it and restart so bootstrap "+
			"rebuilds it", configKeyIndex, st.exists, st.valid, st.unique)
	}
	if st.hasPK {
		if _, err := db.Exec(ctx,
			"ALTER TABLE sage.config DROP CONSTRAINT config_pkey"); err != nil {
			return fmt.Errorf("dropping the old primary key sage.config_pkey: %w", err)
		}
	}
	return nil
}

func ensureConfigAudit(
	ctx context.Context, db bootstrapDB,
) error {
	qctx, cancel := withBudget(ctx, configAuditBudget)
	defer cancel()

	_, err := runMigrationStep(qctx, db, migrationStep{
		name: "ddlConfigAudit", sql: ddlConfigAudit,
	})
	return err
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
