package testdb

import (
	"os"
	"strings"
	"testing"
)

// SkipUnlessLive returns the designated test DSN, skipping t when no
// live test server is configured. Packages whose TestMain routes
// through Run observe the disabled sentinel DSN when
// SAGE_TEST_DATABASE_URL was not set; that counts as "not configured"
// so DB-dependent tests skip with a clear reason instead of failing
// against an unreachable address.
func SkipUnlessLive(t testing.TB) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(EnvName))
	if dsn == "" || dsn == disabledDSN {
		t.Skipf("%s not set; live-Postgres test skipped", EnvName)
	}
	return dsn
}
