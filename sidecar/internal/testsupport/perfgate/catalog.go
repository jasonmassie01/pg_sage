package perfgate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const hotTableRows = 2000

// buildSchemaFn creates one schema's tables (each with a bigserial key,
// so one sequence per table) and their secondary indexes. It is a
// temporary function so the per-schema arguments stay bound parameters.
const buildSchemaFn = `CREATE FUNCTION pg_temp.perfgate_build_schema(
    sch text, first_table int, tables int, idx_count int) RETURNS void
LANGUAGE plpgsql AS $fn$
DECLARE
    i int; k int; tbl text;
    shapes text[] := ARRAY['(tenant_id)', '(created_at)', '(tenant_id, created_at)',
                           '(payload)'];
BEGIN
    EXECUTE format('CREATE SCHEMA IF NOT EXISTS %I', sch);
    FOR i IN first_table .. first_table + tables - 1 LOOP
        tbl := 't_' || lpad(i::text, 4, '0');
        EXECUTE format('CREATE TABLE IF NOT EXISTS %I.%I (id bigserial PRIMARY KEY,
            tenant_id int NOT NULL DEFAULT 0, created_at timestamptz NOT NULL
            DEFAULT now(), payload text)', sch, tbl);
        FOR k IN 1 .. idx_count - 1 LOOP
            EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON %I.%I %s',
                tbl || '_i' || k, sch, tbl, shapes[k]);
        END LOOP;
    END LOOP;
END $fn$`

func appSchema(i int) string   { return fmt.Sprintf("perf_app_%03d", i) }
func cloneSchema(i int) string { return fmt.Sprintf("perf_clone_%03d", i) }
func tableName(i int) string   { return fmt.Sprintf("t_%04d", i) }

// BuildCatalog creates the synthetic monitored catalog: application
// schemas with distinct tables, clone schemas that are identical copies
// of one another (leaked branch or test clones), an idle family of leaked
// test copies (legacy_guard.go) and a few populated hot tables. Each schema is its own transaction to bound the lock table. It
// is idempotent.
func BuildCatalog(ctx context.Context, pool *pgxpool.Pool, s Scale) error {
	if err := s.Validate(); err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("perfgate: acquire connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, buildSchemaFn); err != nil {
		return fmt.Errorf("perfgate: create build function: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(),
			"DROP FUNCTION IF EXISTS pg_temp.perfgate_build_schema(text, int, int, int)")
	}()
	apps := s.Schemas - s.CloneSchemas
	for i := range s.Schemas {
		sch, first := appSchema(i), i*s.TablesPerSchema
		if i >= apps {
			sch, first = cloneSchema(i-apps), 0
		}
		if _, err := conn.Exec(ctx, "SELECT pg_temp.perfgate_build_schema($1, $2, $3, $4)",
			sch, first, s.TablesPerSchema, s.IndexesPerTable); err != nil {
			return fmt.Errorf("perfgate: build schema %s: %w", sch, err)
		}
	}
	if err := buildLeakedClones(ctx, conn); err != nil {
		return err
	}
	return populateHotTables(ctx, conn, s)
}

// populateHotTables gives the workload's tables rows (once).
func populateHotTables(ctx context.Context, conn *pgxpool.Conn, s Scale) error {
	for i := range min(hotTableCount, s.TablesPerSchema) {
		tbl := pgx.Identifier{appSchema(0), tableName(i)}.Sanitize()
		_, err := conn.Exec(ctx, `INSERT INTO `+tbl+` (tenant_id, created_at, payload)
			SELECT 1 + g % 100, now() - g * interval '1 minute', md5(g::text)
			FROM generate_series(1, $1) g
			WHERE NOT EXISTS (SELECT 1 FROM `+tbl+`)`, hotTableRows)
		if err != nil {
			return fmt.Errorf("perfgate: populate %s: %w", tbl, err)
		}
	}
	return nil
}
