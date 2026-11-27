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

// connLimitLabels label the three idle databases of the test.
var connLimitLabels = []string{"f08_idle_a", "f08_idle_b", "f08_idle_c"}

// holdIdleSessions creates the three idle databases on the exclusive
// server t holds, holds one idle session on each until the test ends, and
// returns their names.
func holdIdleSessions(t *testing.T, ctx context.Context) []string {
	t.Helper()
	names := make([]string, 0, len(connLimitLabels))
	for _, label := range connLimitLabels {
		dsn := testdb.ExclusiveDatabase(t, label)
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		openOtherDatabase(t, ctx, dsn)
		names = append(names, cfg.Database)
	}
	return names
}

// client_backends and the later count of client backends both read the
// whole cluster's sessions, so the test holds the exclusive server: on the
// shared one, other packages' sessions close between the two reads.
func TestConnectionLimitClientBackendsExcludeBackgroundProcesses(t *testing.T) {
	dsn := testdb.Exclusive(t, "client_backends counts the whole cluster's sessions")
	c, _ := sageCollectorOn(t, dsn)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	idleDBs := holdIdleSessions(t, ctx)
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
		FROM pg_stat_activity`, idleDBs).Scan(&clients, &idle); err != nil {
		t.Fatalf("count clients: %v", err)
	}
	if idle != len(idleDBs) {
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
