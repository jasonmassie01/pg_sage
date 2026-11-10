package collector

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rca-08: client_backends counts every database's client sessions, the
// connections max_connections limits, and none of the server's background
// processes. Two databases on one server see the same cluster-wide count,
// which is why only the cluster owner raises connections_high.
func TestSystemStatsClientBackendsAreClusterWide(t *testing.T) {
	c, pool := sageCollector(t)
	ctx := context.Background()
	ensureOtherDatabase(t, ctx, pool)
	before, err := c.collectSystem(ctx)
	if err != nil {
		t.Fatalf("collectSystem: %v", err)
	}
	if before.ClientBackends < 1 || before.ClientBackends >= before.TotalBackends {
		t.Fatalf("client_backends = %d of total %d, want at least 1 and fewer than "+
			"total: the checkpointer and other background processes are not clients",
			before.ClientBackends, before.TotalBackends)
	}
	const extra = 3
	for i := 0; i < extra; i++ {
		openOtherDatabase(t, ctx)
	}
	after, err := c.collectSystem(ctx)
	if err != nil {
		t.Fatalf("collectSystem: %v", err)
	}
	if got := after.ClientBackends - before.ClientBackends; got < extra {
		t.Fatalf("client_backends rose by %d after %d sessions on database %s, "+
			"want at least %d: the count is the cluster's", got, extra, otherDatabase, extra)
	}
	if after.TotalBackends-after.ClientBackends < 1 {
		t.Fatalf("background backends = %d, want them still left out",
			after.TotalBackends-after.ClientBackends)
	}
}

// otherDatabase is a second database of the test server.
const otherDatabase = "rca08_other"

// ensureOtherDatabase creates otherDatabase when the server lacks it.
func ensureOtherDatabase(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database "+
		"WHERE datname = $1)", otherDatabase).Scan(&exists); err != nil {
		t.Fatalf("look up database %s: %v", otherDatabase, err)
	}
	if exists {
		return
	}
	if _, err := pool.Exec(ctx, "CREATE DATABASE "+otherDatabase); err != nil {
		t.Fatalf("create database %s: %v", otherDatabase, err)
	}
}

// openOtherDatabase holds one session on otherDatabase until the test
// ends.
func openOtherDatabase(t *testing.T, ctx context.Context) {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = otherDatabase
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to database %s: %v", otherDatabase, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var db string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil ||
		db != otherDatabase {
		t.Fatalf("second database session on %q: %v", db, err)
	}
}

// Every version variant selects client_backends last, after is_replica,
// so the scan order of scanSystemStats matches. The variants read their
// checkpoint view in a trailing FROM (forecast-03), so "last" is: no
// column alias follows client_backends.
func TestSystemStatsSQLEndsWithClientBackends(t *testing.T) {
	for name, sql := range map[string]string{"14": systemStatsSQL14, "17": systemStatsSQL17,
		"18": systemStatsSQL18} {
		replica := strings.Index(sql, "AS is_replica")
		client := strings.Index(sql, "AS client_backends")
		if replica < 0 || client < replica ||
			strings.Contains(sql[client+len("AS client_backends"):], " AS ") {
			t.Errorf("PG%s system stats SQL does not end with client_backends after "+
				"is_replica", name)
		}
		if !strings.Contains(sql, "backend_type = 'client backend') AS client_backends") {
			t.Errorf("PG%s client_backends does not filter client backends", name)
		}
	}
}
