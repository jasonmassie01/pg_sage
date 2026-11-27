package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Happy path on real PostgreSQL as a non-superuser: self-managed, no error,
// and one statement per probe (five, under the six the entry allows).
func TestDetectCloudEnvSelfManagedAsNonSuperuser(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "probe_ok")
	reader := probeReaderDSN(t, dsn)
	recorder := &testdb.QueryRecorder{}
	p := probePool(t, reader, recorder)
	var super bool
	if err := p.QueryRow(context.Background(),
		"SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if super {
		t.Fatal("probe role is a superuser; the test needs an ordinary role")
	}
	recorder.Reset()

	got, err := detectCloudEnvContext(context.Background(), p)

	if got != providerSelfManaged || err != nil {
		t.Fatalf("detectCloudEnvContext = %q, %v; want self-managed, nil", got, err)
	}
	if n := len(recorder.Matching("")); n == 0 || n >= 6 {
		t.Fatalf("probe statements = %d, want 1..5", n)
	}
	if detectCloudEnv(p) != providerSelfManaged {
		t.Fatal("detectCloudEnv disagrees with detectCloudEnvContext")
	}
}

// Error propagation on real PostgreSQL (failed before clouds-05a): the
// database refuses every connection, so each probe errors; the answer is
// unknown, never self-managed.
func TestDetectCloudEnvUnreachableDatabaseIsUnknown(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "probe_down")
	p := probePool(t, dsn, nil)
	admin := probeAdminPool(t)
	database := probeDatabaseName(t, dsn)
	blockConnections(t, admin, database)
	waits := 0
	d := newProviderDetector(p)
	d.sleep = func(ctx context.Context, _ time.Duration) error { waits++; return ctx.Err() }

	got, err := d.detect(context.Background())

	if got != providerUnknown {
		t.Fatalf("provider = %q, want unknown", got)
	}
	if err == nil || !strings.Contains(err.Error(), "aurora") {
		t.Fatalf("error = %v, want the failing aurora probe named", err)
	}
	if waits != 3 {
		t.Fatalf("retries = %d, want 3", waits)
	}
	allowConnections(t, admin, database)
	if got, err := newProviderDetector(p).probeOnce(context.Background()); got !=
		providerSelfManaged || err != nil {
		t.Fatalf("after recovery: probeOnce = %q, %v; want self-managed, nil", got, err)
	}
}

func probeAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	return admin
}

func probePool(t *testing.T, dsn string, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	poolCfg.ConnConfig.Tracer = tracer
	p, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(p.Close)
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return p
}

// probeReaderDSN creates an ordinary login role and returns dsn as it.
func probeReaderDSN(t *testing.T, dsn string) string {
	t.Helper()
	admin := probeAdminPool(t)
	role := fmt.Sprintf("probe_reader_%d", time.Now().UnixNano())
	ctx := context.Background()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+
		" LOGIN NOSUPERUSER PASSWORD 'probe-reader-test'"); err != nil {
		t.Fatalf("create probe role: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+role); err != nil {
			t.Errorf("drop probe role: %v", err)
		}
	})
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	parsed.User = url.UserPassword(role, "probe-reader-test")
	return parsed.String()
}

func probeDatabaseName(t *testing.T, dsn string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	return strings.TrimPrefix(parsed.Path, "/")
}

// blockConnections makes database refuse new connections and ends the
// existing ones, as an outage of that database would.
func blockConnections(t *testing.T, admin *pgxpool.Pool, database string) {
	t.Helper()
	ctx := context.Background()
	ident := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+ident+" ALLOW_CONNECTIONS false"); err != nil {
		t.Fatalf("block connections: %v", err)
	}
	t.Cleanup(func() { allowConnections(t, admin, database) })
	if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = $1 AND pid <> pg_backend_pid()`, database); err != nil {
		t.Fatalf("terminate backends: %v", err)
	}
}

func allowConnections(t *testing.T, admin *pgxpool.Pool, database string) {
	t.Helper()
	ident := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(context.Background(),
		"ALTER DATABASE "+ident+" ALLOW_CONNECTIONS true"); err != nil {
		t.Errorf("allow connections: %v", err)
	}
}
