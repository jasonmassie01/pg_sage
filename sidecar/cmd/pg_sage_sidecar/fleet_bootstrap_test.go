package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Nil/empty: without action_dsn no second pool is built (regression
// guard for the single-identity default).
func TestOpenActionPool_NoActionDSNBuildsNoPool(t *testing.T) {
	db := config.DatabaseConfig{Name: "orders", Host: "127.0.0.1", Port: 1,
		User: "observer", Database: "orders"}
	p, err := openActionPool(db.ConnString(), db)
	if err != nil || p != nil {
		t.Fatalf("openActionPool(no action_dsn) = (%v, %v), want (nil, nil)", p, err)
	}
}

// Invalid input: an action_dsn on another database is refused before any
// connection, even when config validation was bypassed (meta-db records,
// reloads).
func TestOpenActionPool_RefusesAnotherDatabase(t *testing.T) {
	db := config.DatabaseConfig{Name: "orders", Host: "127.0.0.1", Port: 1,
		User: "observer", Database: "orders",
		ActionDSN: "postgres://actor:pw@127.0.0.1:1/billing?sslmode=disable"}
	p, err := openActionPool(db.ConnString(), db)
	if p != nil {
		p.Close()
		t.Fatal("openActionPool built a pool for another database")
	}
	if !errors.Is(err, config.ErrActionDSNTarget) {
		t.Fatalf("openActionPool = %v, want ErrActionDSNTarget", err)
	}
}

// Error propagation: an unreachable action identity still gets its
// (lazy, two-connection) pool, so observation starts; each change then
// fails with "action identity unavailable" (executor.checkActionIdentity).
func TestOpenActionPool_UnreachableIsLazy(t *testing.T) {
	db := config.DatabaseConfig{Name: "orders", Host: "127.0.0.1", Port: 1,
		User: "observer", Database: "orders",
		ActionDSN: "postgres://actor:pw@127.0.0.1:1/orders?sslmode=disable&" +
			"connect_timeout=1"}
	p, err := openActionPool(db.ConnString(), db)
	if err != nil || p == nil {
		t.Fatalf("openActionPool(unreachable) = (%v, %v), want a lazy pool", p, err)
	}
	defer p.Close()
	pc := p.Config()
	if pc.MaxConns != executor.ActionPoolMaxConns() || pc.MinConns != 0 ||
		pc.ConnConfig.User != "actor" {
		t.Fatalf("action pool = max %d min %d user %q; want %d, 0, actor", pc.MaxConns,
			pc.MinConns, pc.ConnConfig.User, executor.ActionPoolMaxConns())
	}
	if app := pc.ConnConfig.RuntimeParams["application_name"]; !strings.Contains(app,
		"pg_sage") {
		t.Fatalf("action pool application_name = %q, want pg_sage's", app)
	}
	if !poolClosed(p) {
		t.Fatal("an unreachable action identity answered a ping")
	}
}

// actionRoleFixture creates a login role on the test server that is a
// member of pg_signal_backend, and returns its name and DSN.
func actionRoleFixture(t *testing.T, dsn string) (string, string) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	role := fmt.Sprintf("sage_act_boot_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+
		" LOGIN PASSWORD 'act-pw' IN ROLE pg_signal_backend"); err != nil {
		t.Fatalf("create action role: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = admin.Exec(c, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE usename = $1`, role)
		_, _ = admin.Exec(c, "DROP ROLE IF EXISTS "+role)
	})
	conn := admin.Config().ConnConfig
	u := url.URL{Scheme: "postgres", User: url.UserPassword(role, "act-pw"),
		Host: net.JoinHostPort(conn.Host, strconv.Itoa(int(conn.Port))),
		Path: "/" + conn.Database, RawQuery: "sslmode=disable"}
	return role, u.String()
}

// sidecarUsers counts the distinct roles of pg_sage's own sessions on the
// fixture database, among the observation and action roles.
func sidecarUsers(t *testing.T, p *pgxpool.Pool, roles ...string) map[string]bool {
	t.Helper()
	rows, err := p.Query(context.Background(), `SELECT DISTINCT usename::text
		FROM pg_stat_activity WHERE datname = current_database()
		  AND application_name ILIKE '%pg_sage%' AND usename = ANY($1)`, roles)
	if err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[u] = true
	}
	return out
}

// Integration (CHECK-02): a YAML fleet database with action_dsn runs two
// identities, the executor takes the action pool, and removing the
// database closes both pools; the same database without it runs one.
func TestFleetBootstrap_ActionIdentityPools(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	role, actionDSN := actionRoleFixture(t, dsn)
	base := parityBaseConfig(t, dsn)
	for _, withAction := range []bool{true, false} {
		t.Run(fmt.Sprintf("action_dsn=%t", withAction), func(t *testing.T) {
			inst := buildActionFleet(t, base, withAction, actionDSN)
			assertActionIdentity(t, inst, base.Postgres.User, role, withAction)
		})
	}
}

func buildActionFleet(t *testing.T, base *config.Config, withAction bool,
	actionDSN string) *fleet.DatabaseInstance {
	t.Helper()
	preserveParityGlobals(t, base)
	cfg.Mode = "fleet"
	name := base.Postgres.Database
	db := parityDatabaseConfig(base, name)
	if withAction {
		db.ActionDSN = actionDSN
	}
	cfg.Databases = []config.DatabaseConfig{db}
	initFleetMultiDB()
	inst := fleetMgr.GetInstance(name)
	if inst == nil || inst.Pool == nil {
		t.Fatalf("fleet registered no connected runtime for %s", name)
	}
	control := inst.Pool
	t.Cleanup(func() {
		_, _ = control.Exec(context.Background(),
			"DELETE FROM sage.databases WHERE name = $1", name)
	})
	return inst
}

func assertActionIdentity(t *testing.T, inst *fleet.DatabaseInstance,
	observer, actor string, withAction bool) {
	t.Helper()
	if got := inst.Executor.HasActionIdentity(); got != withAction {
		t.Fatalf("executor HasActionIdentity = %t, want %t", got, withAction)
	}
	if (inst.ActionPool != nil) != withAction {
		t.Fatalf("instance action pool = %v, want present=%t", inst.ActionPool, withAction)
	}
	if !withAction {
		users := sidecarUsers(t, inst.Pool, observer, actor)
		if users[actor] || !users[observer] {
			t.Fatalf("pg_sage sessions run as %v, want only %s", users, observer)
		}
		return
	}
	if got := inst.ActionPool.Config().MaxConns; got != executor.ActionPoolMaxConns() {
		t.Fatalf("action pool MaxConns = %d, want %d", got, executor.ActionPoolMaxConns())
	}
	if got := inst.ActionPool.Config().MinConns; got != 0 {
		t.Fatalf("action pool MinConns = %d, want 0", got)
	}
	ctx := context.Background()
	if err := inst.ActionPool.Ping(ctx); err != nil {
		t.Fatalf("action pool ping: %v", err)
	}
	conn, err := inst.ActionPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire action conn: %v", err)
	}
	users := sidecarUsers(t, inst.Pool, observer, actor)
	conn.Release()
	if !users[actor] || !users[observer] {
		t.Fatalf("pg_sage sessions run as %v, want both %s and %s", users, observer, actor)
	}
	removeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := fleet.ShutdownInstance(removeCtx, inst); err != nil {
		t.Fatalf("shutdown instance: %v", err)
	}
	if !poolClosed(inst.ActionPool) || !poolClosed(inst.Pool) {
		t.Fatal("shutting the instance down left a pool open")
	}
}

// The posture checks (AP-14) learn the action role from the runtime's
// action pool, and nothing without one.
func TestActionRoleNamesTheActionPoolUser(t *testing.T) {
	if got := actionRole(nil); got != "" {
		t.Fatalf("actionRole(nil) = %q, want empty", got)
	}
	db := config.DatabaseConfig{Name: "orders", Host: "127.0.0.1", Port: 1,
		User: "observer", Database: "orders",
		ActionDSN: "postgres://actor:pw@127.0.0.1:1/orders?sslmode=disable&" +
			"connect_timeout=1"}
	p, err := openActionPool(db.ConnString(), db)
	if err != nil {
		t.Fatalf("openActionPool: %v", err)
	}
	defer p.Close()
	if got := actionRole(p); got != "actor" {
		t.Fatalf("actionRole = %q, want actor", got)
	}
	rt := &databaseRuntime{spec: databaseRuntimeSpec{ActionPool: p}, cfg: config.DefaultConfig()}
	if got := rt.newPostureMonitorConfig().ActionRole; got != "actor" {
		t.Fatalf("posture monitor action role = %q, want actor", got)
	}
}

// The agent kill switch signals on the instance's action pool, and on its
// pool without one.
func TestKillTargetTakesTheActionPool(t *testing.T) {
	db := config.DatabaseConfig{Name: "orders", Host: "127.0.0.1", Port: 1,
		User: "observer", Database: "orders",
		ActionDSN: "postgres://actor:pw@127.0.0.1:1/orders?sslmode=disable&" +
			"connect_timeout=1"}
	act, err := openActionPool(db.ConnString(), db)
	if err != nil {
		t.Fatalf("openActionPool: %v", err)
	}
	defer act.Close()
	obs, err := pgxpool.New(context.Background(), db.ConnString()+"&connect_timeout=1")
	if err != nil {
		t.Fatalf("observation pool: %v", err)
	}
	defer obs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	split := killTarget(ctx, "orders", &fleet.DatabaseInstance{Name: "orders", Pool: obs,
		ActionPool: act, Config: db})
	if split.SignalPool != act || split.Pool != obs {
		t.Fatalf("kill target pools = %p/%p, want signal %p on pool %p", split.SignalPool,
			split.Pool, act, obs)
	}
	single := killTarget(ctx, "orders", &fleet.DatabaseInstance{Name: "orders", Pool: obs,
		Config: db})
	if single.SignalPool != nil || single.Pool != obs {
		t.Fatalf("single identity kill target = %+v", single)
	}
}
