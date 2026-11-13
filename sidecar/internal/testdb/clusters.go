package testdb

import (
	"os"
	"strings"
	"testing"
)

const (
	// SecondClusterEnvName names a second, disposable PostgreSQL server
	// (not the one SAGE_TEST_DATABASE_URL names), for tests that need a
	// database on another cluster, such as a game-day target.
	SecondClusterEnvName = "SAGE_TEST_SECOND_CLUSTER_URL"
	// ReplicaEnvName names a streaming replica of the server that
	// SAGE_TEST_DATABASE_URL names.
	ReplicaEnvName = "SAGE_TEST_REPLICA_URL"
)

// CreateDatabaseOnSecondCluster creates one disposable database on the
// second test server and force-drops it when the test ends. It skips when
// no second server is configured.
func CreateDatabaseOnSecondCluster(t testing.TB, label string) string {
	t.Helper()
	base := strings.TrimSpace(os.Getenv(SecondClusterEnvName))
	if base == "" {
		t.Skipf("%s not set; this test needs a second PostgreSQL server", SecondClusterEnvName)
	}
	name, err := extraDatabaseName(label)
	if err != nil {
		t.Fatalf("name second-cluster test database: %v", err)
	}
	dsn, err := createFixture(base, name, "second-cluster")
	if err != nil {
		t.Fatalf("create second-cluster test database: %v", err)
	}
	t.Cleanup(func() {
		if err := dropFixture(base, name); err != nil {
			t.Errorf("drop second-cluster test database: %v", err)
		}
	})
	return dsn
}

// ReplicaDSN is the streaming replica of the designated test server. It
// skips when none is configured.
func ReplicaDSN(t testing.TB) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(ReplicaEnvName))
	if dsn == "" {
		t.Skipf("%s not set; this test needs a streaming replica of the test server",
			ReplicaEnvName)
	}
	return dsn
}
