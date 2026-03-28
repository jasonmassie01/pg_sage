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
    changed_by  INT REFERENCES sage.users(id),
    changed_at  TIMESTAMPTZ DEFAULT now()
);
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
	if err := addConfigColumns(ctx, pool); err != nil {
		return fmt.Errorf("config columns: %w", err)
	}
	if err := ensureConfigAudit(ctx, pool); err != nil {
		return fmt.Errorf("config_audit: %w", err)
	}
	return nil
}

func addConfigColumns(
	ctx context.Context, pool *pgxpool.Pool,
) error {
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Add database_id column if missing.
	_, err := pool.Exec(qctx, `
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
	_, err = pool.Exec(qctx, `
		DO $$ BEGIN
			ALTER TABLE sage.config
				ADD COLUMN updated_by_user_id INT
				REFERENCES sage.users(id);
		EXCEPTION
			WHEN duplicate_column THEN NULL;
		END $$;
	`)
	if err != nil {
		return fmt.Errorf("adding updated_by_user_id: %w", err)
	}

	return addConfigCompositeIndex(qctx, pool)
}

func addConfigCompositeIndex(
	ctx context.Context, pool *pgxpool.Pool,
) error {
	_, err := pool.Exec(ctx, `
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
	ctx context.Context, pool *pgxpool.Pool,
) error {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err := pool.Exec(qctx, ddlConfigAudit)
	if err != nil {
		return fmt.Errorf("creating config_audit: %w", err)
	}
	return nil
}
