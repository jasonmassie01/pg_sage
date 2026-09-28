package testdb

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RequireServerVersion skips t when the server is older than minVersion
// (server_version_num form, e.g. 160000), naming the feature so the CI skip
// budget can allow version-gated skips explicitly. A server whose version
// cannot be read fails the test instead of skipping it.
func RequireServerVersion(t testing.TB, pool *pgxpool.Pool, minVersion int, feature string) {
	t.Helper()
	var version int
	err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version)
	if err != nil {
		t.Fatalf("read server_version_num: %v", err)
		return
	}
	if version < minVersion {
		t.Skipf("%s requires PostgreSQL %d+ (server_version_num %d)",
			feature, minVersion/10000, version)
	}
}
