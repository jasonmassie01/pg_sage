package cases

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: candidateFromFinding is a pure function.

// Maintenance, data removal, privileges and cluster-wide changes are not
// an application's schema migrations: they stay advice.
func TestCandidateFromFindingKeepsNonSchemaChangesAsAdvice(t *testing.T) {
	for _, sql := range []string{
		"VACUUM FULL public.orders", "REINDEX TABLE public.orders",
		"TRUNCATE public.orders", "ALTER SYSTEM SET work_mem = '64MB'",
		"ALTER ROLE app SET statement_timeout = '5s'", "CREATE ROLE reporting",
		"DROP DATABASE scratch", "GRANT SELECT ON public.orders TO reporting",
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
		"ALTER DEFAULT PRIVILEGES GRANT SELECT ON TABLES TO reporting",
		"CREATE SUBSCRIPTION s CONNECTION 'host=x' PUBLICATION p",
		"CREATE TABLESPACE fast LOCATION '/ssd'", "COMMENT ON ROLE app IS 'x'",
		"alter  system  reset all", "CREATE USER MAPPING FOR app SERVER s",
		"CREATE EVENT TRIGGER e ON ddl_command_start EXECUTE FUNCTION f()",
	} {
		_, ok := candidateFromFinding(prFinding{ID: 5, Title: "t", SQL: sql}, noContract)
		require.False(t, ok, "%s is not a migration PR", sql)
	}
	for _, sql := range []string{
		"CREATE UNIQUE INDEX orders_ref ON public.orders (ref)",
		"COMMENT ON TABLE public.orders IS 'x'", "DROP INDEX public.orders_old",
		"CREATE OR REPLACE VIEW v AS SELECT 1", "ALTER TABLE t ADD COLUMN c int",
	} {
		c, ok := candidateFromFinding(prFinding{ID: 5, Title: "t", SQL: sql}, noContract)
		require.True(t, ok, "%s is a migration", sql)
		require.Equal(t, PRKindMigration, c.Kind)
	}
}
