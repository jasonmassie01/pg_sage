package safetybench

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fixtureSchema is the schema the read-only corpus writes its seed tables
// into. Everything the corpus could touch lives here so the checksum set is
// known and bounded.
const fixtureSchema = "sb_fixture"

// readOnlyRole is the privilege-based read-only role the privRoleDesign
// drops to with SET LOCAL ROLE.
const readOnlyRole = "sb_readonly"

// fixtureTables are the seed tables the corpus runs against; the checksum
// snapshot covers exactly these.
func fixtureTables() []string {
	return []string{
		quoteIdent(fixtureSchema) + "." + quoteIdent("widgets"),
		quoteIdent(fixtureSchema) + "." + quoteIdent("ledger"),
	}
}

// fixtureDDL builds the corpus fixture: a seed schema with two small
// tables and a sequence, and the read-only role. It drops and recreates the
// schema so a reused database is reset to a known state.
// /* pg_sage safety_bench v1 */
func fixtureDDL() []string {
	sch := quoteIdent(fixtureSchema)
	return []string{
		"DROP SCHEMA IF EXISTS " + sch + " CASCADE",
		"CREATE SCHEMA " + sch,
		"CREATE TABLE " + sch + ".widgets (id int PRIMARY KEY, qty int NOT NULL)",
		"INSERT INTO " + sch + ".widgets (id, qty) VALUES (1, 10), (2, 20), (3, 30)",
		"CREATE TABLE " + sch + ".ledger (id serial PRIMARY KEY, note text NOT NULL)",
		"INSERT INTO " + sch + ".ledger (note) VALUES ('opening'), ('balance')",
		"CREATE SEQUENCE IF NOT EXISTS " + sch + ".counter",
		createReadOnlyRoleSQL(),
	}
}

// createReadOnlyRoleSQL makes the read-only role idempotently. A DO block
// keeps it safe to re-run without a CREATE ROLE IF NOT EXISTS (which
// PostgreSQL lacks).
func createReadOnlyRoleSQL() string {
	return `DO $sb$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '` + readOnlyRole + `') THEN
    CREATE ROLE ` + quoteIdent(readOnlyRole) + ` NOLOGIN;
  END IF;
END
$sb$`
}

// grantReadOnly grants the read-only role exactly the read privileges it
// needs on the fixture schema, and nothing that would let it write. It runs
// after the schema exists. /* pg_sage safety_bench v1 */
func grantReadOnly(ctx context.Context, owner *pgxpool.Pool) error {
	sch := quoteIdent(fixtureSchema)
	role := quoteIdent(readOnlyRole)
	stmts := []string{
		"GRANT USAGE ON SCHEMA " + sch + " TO " + role,
		"GRANT SELECT ON ALL TABLES IN SCHEMA " + sch + " TO " + role,
		// Deliberately no GRANT on sequences or INSERT/UPDATE/DELETE: the
		// read-only role must fail a write with 42501.
	}
	for _, s := range stmts {
		if _, err := owner.Exec(ctx, s); err != nil {
			return fmt.Errorf("grant read-only (%.40q): %w", s, err)
		}
	}
	return nil
}

// PrepareReadOnly builds the corpus fixture and read-only role on owner's
// database. Call it once per database before running cases. SET ROLE needs
// no CONNECT grant because it never opens a new connection.
func PrepareReadOnly(ctx context.Context, owner *pgxpool.Pool) error {
	for _, stmt := range fixtureDDL() {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("fixture setup (%.40q): %w", stmt, err)
		}
	}
	return grantReadOnly(ctx, owner)
}

// quoteIdent quotes a SQL identifier by doubling embedded quotes. The
// identifiers here are compile-time constants, so this is defence in depth.
func quoteIdent(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
