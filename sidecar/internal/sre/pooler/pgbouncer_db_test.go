package pooler

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Against a real PgBouncer (CHECK-04): its pool is exhausted by one
// long-running client, a second client queues, and the probe reads the
// queue from the admin console. The test runs when a PgBouncer is named:
//
//   - SAGE_TEST_PGBOUNCER_ADMIN_URL: a stats user's DSN for the admin
//     console (database "pgbouncer").
//   - SAGE_TEST_PGBOUNCER_CLIENT_URL: a client DSN through the same
//     PgBouncer to a pool whose pool_size is 1.

func pgbouncerEnv(t *testing.T) (admin, client string) {
	t.Helper()
	admin, client = os.Getenv("SAGE_TEST_PGBOUNCER_ADMIN_URL"),
		os.Getenv("SAGE_TEST_PGBOUNCER_CLIENT_URL")
	if admin == "" || client == "" {
		t.Skip("SAGE_TEST_PGBOUNCER_ADMIN_URL and SAGE_TEST_PGBOUNCER_CLIENT_URL not set: " +
			"no PgBouncer to read")
	}
	return admin, client
}

func clientConn(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse client dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect through PgBouncer: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func poolOf(t *testing.T, res probes.Result, db string) probes.PoolerPool {
	t.Helper()
	pools, failures, err := probes.PoolerPoolsOf(res)
	if err != nil || len(failures) != 0 {
		t.Fatalf("pooler_pools = %+v (%v, failures %+v)", res, err, failures)
	}
	for _, p := range pools {
		if p.Database == db {
			return p
		}
	}
	t.Fatalf("pool %q not in %+v", db, pools)
	return probes.PoolerPool{}
}

func TestPgBouncer_ReadsAQueueAtAnExhaustedPool(t *testing.T) {
	admin, client := pgbouncerEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cu, err := url.Parse(client)
	if err != nil {
		t.Fatalf("client dsn: %v", err)
	}
	db := strings.TrimPrefix(cu.Path, "/")
	s := newSource(t, DialPgBouncer, Config{Name: "pgb-test", DSN: admin,
		Timeout: 2 * time.Second})
	holder := clientConn(t, ctx, client)
	waiter := clientConn(t, ctx, client)
	held := make(chan error, 1)
	go func() {
		_, err := holder.Exec(ctx, "SELECT pg_sleep(4)")
		held <- err
	}()
	time.Sleep(300 * time.Millisecond) // the holder takes the only server connection
	queued := make(chan error, 1)
	go func() {
		_, err := waiter.Exec(ctx, "SELECT 1")
		queued <- err
	}()
	time.Sleep(1500 * time.Millisecond)
	p := poolOf(t, s.Probe(ctx, probes.Args{}), db)
	if p.ClientWaiting < 1 || p.ServerActive < 1 || p.ServerIdle != 0 || p.MaxWaitS < 1 {
		t.Fatalf("exhausted pool = %+v, want a waiting client for at least 1 s", p)
	}
	if err := <-held; err != nil {
		t.Fatalf("holder: %v", err)
	}
	if err := <-queued; err != nil {
		t.Fatalf("queued client: %v", err)
	}
	after := poolOf(t, s.Probe(ctx, probes.Args{}), db)
	if after.ClientWaiting != 0 {
		t.Fatalf("pool after the queue drained = %+v", after)
	}
}

// Wrong credentials are no_privilege with a reason, and the password
// never appears in the result.
func TestPgBouncer_WrongPasswordIsNoPrivilege(t *testing.T) {
	admin, _ := pgbouncerEnv(t)
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("admin dsn: %v", err)
	}
	u.User = url.UserPassword(u.User.Username(), "wrong-Pa55-CANARY")
	s := newSource(t, DialPgBouncer, Config{Name: "pgb-test", DSN: u.String(),
		Timeout: 2 * time.Second})
	res := s.Probe(context.Background(), probes.Args{})
	if res.Status != probes.StatusNoPrivilege || res.Reason != ReasonAuthFailed {
		t.Fatalf("wrong password = %+v, want no_privilege %s", res, ReasonAuthFailed)
	}
	raw, _ := res.Payload()
	if strings.Contains(string(raw), "CANARY") {
		t.Fatalf("result leaks the password: %s", raw)
	}
}

// Nothing listens: unreachable, in bounded time.
func TestPgBouncer_NothingListening(t *testing.T) {
	admin, _ := pgbouncerEnv(t)
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("admin dsn: %v", err)
	}
	u.Host = u.Hostname() + ":1"
	s := newSource(t, DialPgBouncer, Config{Name: "pgb-test", DSN: u.String(),
		Timeout: 2 * time.Second})
	start := time.Now()
	res := s.Probe(context.Background(), probes.Args{})
	if res.Status != probes.StatusError || (res.Reason != ReasonUnreachable &&
		res.Reason != ReasonTimeout) || time.Since(start) > 5*time.Second {
		t.Fatalf("nothing listening = %+v after %s", res, time.Since(start))
	}
}
