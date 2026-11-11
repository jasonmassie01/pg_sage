package collector

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// forecast-08: the connection forecast counts client backends against
// max_connections less the reserved slots, so the system snapshot carries
// both. client_backends leaves the server's background processes out;
// reserved_connections is superuser_reserved_connections plus, on PG 16+,
// reserved_connections.

// connLimitDatabases are the three idle databases of the test.
var connLimitDatabases = []string{"f08_idle_a", "f08_idle_b", "f08_idle_c"}

// holdIdleSessions creates each database when missing and holds one idle
// session on it until the test ends.
func holdIdleSessions(t *testing.T, ctx context.Context, admin *pgx.Conn) {
	t.Helper()
	cfg, err := pgx.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range connLimitDatabases {
		var exists bool
		if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database "+
			"WHERE datname = $1)", db).Scan(&exists); err != nil {
			t.Fatalf("look up database %s: %v", db, err)
		}
		if !exists {
			sql := "CREATE DATABASE " + pgx.Identifier{db}.Sanitize()
			if _, err := admin.Exec(ctx, sql); err != nil {
				t.Fatalf("create database %s: %v", db, err)
			}
		}
		c := cfg.Copy()
		c.Database = db
		conn, err := pgx.ConnectConfig(ctx, c)
		if err != nil {
			t.Fatalf("connect to %s: %v", db, err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
	}
}

func TestConnectionLimitClientBackendsExcludeBackgroundProcesses(t *testing.T) {
	c, _ := sageCollector(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	holdIdleSessions(t, ctx, admin)
	var background int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
		WHERE backend_type IN ('checkpointer', 'walwriter', 'autovacuum launcher')`).
		Scan(&background); err != nil {
		t.Fatalf("count background processes: %v", err)
	}
	if background != 3 {
		t.Fatalf("server shows %d of checkpointer, walwriter and autovacuum launcher, "+
			"want 3: the test needs them to tell clients from background", background)
	}
	s, err := c.collectSystem(ctx)
	if err != nil {
		t.Fatalf("collectSystem: %v", err)
	}
	var clients, idle int
	if err := admin.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE backend_type = 'client backend'),
		count(*) FILTER (WHERE backend_type = 'client backend' AND datname = ANY($1))
		FROM pg_stat_activity`, connLimitDatabases).Scan(&clients, &idle); err != nil {
		t.Fatalf("count clients: %v", err)
	}
	if idle != len(connLimitDatabases) {
		t.Fatalf("idle sessions on the three databases = %d, want 3", idle)
	}
	// The collector's own reading session is not counted (pid <>
	// pg_backend_pid()); this test's admin session is.
	if s.ClientBackends < idle || s.ClientBackends > clients {
		t.Errorf("client_backends = %d, want between the %d idle sessions and the %d "+
			"client backends", s.ClientBackends, idle, clients)
	}
	if s.TotalBackends-s.ClientBackends < background {
		t.Errorf("total %d - client %d < %d background processes: they were counted "+
			"as clients", s.TotalBackends, s.ClientBackends, background)
	}
}

func TestConnectionLimitReservedConnections(t *testing.T) {
	c, pool := sageCollector(t)
	ctx := context.Background()
	var version, superuser int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,
		current_setting('superuser_reserved_connections')::int`).
		Scan(&version, &superuser); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	want := superuser
	if version >= 160000 {
		var reserved int
		if err := pool.QueryRow(ctx, "SELECT current_setting('reserved_connections')::int").
			Scan(&reserved); err != nil {
			t.Fatalf("PG %d lacks reserved_connections: %v", version, err)
		}
		want += reserved
	}
	if superuser != 3 {
		t.Fatalf("superuser_reserved_connections = %d, want the default 3 on a test server",
			superuser)
	}
	s, err := c.collectSystem(ctx)
	if err != nil {
		t.Fatalf("collectSystem: %v", err)
	}
	if s.ReservedConnections != want {
		t.Fatalf("reserved_connections = %d on PG %d, want %d", s.ReservedConnections,
			version, want)
	}
	if version < 160000 && s.ReservedConnections != superuser {
		t.Errorf("PG %d: reserved_connections = %d, want superuser_reserved_connections %d",
			version, s.ReservedConnections, superuser)
	}
	t.Logf("PG %d reserved_connections = %d", version, want)
}
