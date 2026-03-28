package schema

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const ddlDatabases = `
CREATE TABLE IF NOT EXISTS sage.databases (
    id              SERIAL PRIMARY KEY,
    name            TEXT UNIQUE NOT NULL,
    host            TEXT NOT NULL,
    port            INT NOT NULL DEFAULT 5432,
    database_name   TEXT NOT NULL,
    username        TEXT NOT NULL,
    password_enc    BYTEA NOT NULL,
    sslmode         TEXT NOT NULL DEFAULT 'require',
    max_connections INT NOT NULL DEFAULT 2,
    enabled         BOOLEAN DEFAULT true,
    tags            JSONB DEFAULT '{}',
    trust_level     TEXT NOT NULL DEFAULT 'observation',
    execution_mode  TEXT NOT NULL DEFAULT 'approval',
    created_at      TIMESTAMPTZ DEFAULT now(),
    created_by      INT,
    updated_at      TIMESTAMPTZ DEFAULT now()
);
`

// EnsureDatabasesTable creates sage.databases if it doesn't exist.
func EnsureDatabasesTable(ctx context.Context, pool *pgxpool.Pool) error {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err := pool.Exec(qctx, ddlDatabases)
	if err != nil {
		return fmt.Errorf("creating sage.databases: %w", err)
	}
	return nil
}
