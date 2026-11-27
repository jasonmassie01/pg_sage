package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// identityFixture is a monitored schema owned by an ordinary owner role,
// an observation role (pg_monitor and SELECT, no owner membership, no
// pg_signal_backend), an action role (owner member and pg_signal_backend)
// and an application role whose sessions pg_sage may signal.
type identityFixture struct {
	super, obs, act         *pgxpool.Pool
	schema, owner, observer string
	actor, app, dsn         string
}

func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	super, ctx := requireDB(t)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	f := &identityFixture{super: super, schema: "ap_s" + sfx, owner: "ap_owner" + sfx,
		observer: "ap_obs" + sfx, actor: "ap_act" + sfx, app: "ap_app" + sfx,
		dsn: testdb.SkipUnlessLive(t)}
	stmts := []string{
		"CREATE ROLE " + f.owner + " NOLOGIN",
		"CREATE ROLE " + f.observer + " LOGIN PASSWORD 'obs' IN ROLE pg_monitor",
		// pg_read_all_stats: a backend signal matches the session's query
		// and start times in pg_stat_activity, which a role sees only for
		// its own sessions without it.
		"CREATE ROLE " + f.actor + " LOGIN PASSWORD 'act' IN ROLE " + f.owner +
			", pg_signal_backend, pg_read_all_stats",
		"CREATE ROLE " + f.app + " LOGIN PASSWORD 'app'",
		"CREATE SCHEMA " + f.schema + " AUTHORIZATION " + f.owner,
		"GRANT USAGE ON SCHEMA " + f.schema + " TO " + f.observer,
	}
	for i := 1; i <= 3; i++ {
		table := fmt.Sprintf("%s.t%d", f.schema, i)
		stmts = append(stmts, "CREATE TABLE "+table+" (id int)",
			"INSERT INTO "+table+" SELECT generate_series(1, 100)",
			"ALTER TABLE "+table+" OWNER TO "+f.owner,
			"GRANT SELECT ON "+table+" TO "+f.observer)
	}
	for _, s := range stmts {
		if _, err := super.Exec(ctx, s); err != nil {
			t.Fatalf("fixture %q: %v", s, err)
		}
	}
	t.Cleanup(f.drop)
	f.obs, f.act = f.login(t, f.observer, "obs"), f.login(t, f.actor, "act")
	return f
}

func (f *identityFixture) login(t *testing.T, role, password string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, password
	cfg.MaxConns, cfg.MinConns = ActionPoolMaxConns(), 0 // the production sizing
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool for %s: %v", role, err)
	}
	t.Cleanup(p.Close)
	return p
}

func (f *identityFixture) drop() {
	ctx := context.Background()
	if f.obs != nil {
		f.obs.Close()
	}
	if f.act != nil {
		f.act.Close()
	}
	roles := []string{f.observer, f.actor, f.app}
	_, _ = f.super.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE usename = ANY($1)`, roles)
	_, _ = f.super.Exec(ctx, "DROP SCHEMA IF EXISTS "+f.schema+" CASCADE")
	for _, r := range append(roles, f.owner) {
		_, _ = f.super.Exec(ctx, "DROP ROLE IF EXISTS "+r)
	}
}

func identityFinding(sql, table string) analyzer.Finding {
	return analyzer.Finding{Category: "missing_index", ObjectType: "table",
		ObjectIdentifier: table, RecommendedSQL: sql}
}

func requireSQLState(t *testing.T, what string, err error, code string) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != code {
		t.Fatalf("%s = %v, want SQLSTATE %s", what, err, code)
	}
}

func indexIsValid(t *testing.T, f *identityFixture, index string) bool {
	t.Helper()
	var valid bool
	err := f.super.QueryRow(context.Background(), `SELECT COALESCE((SELECT indisvalid
		FROM pg_index WHERE indexrelid = to_regclass($1)), false)`, index).Scan(&valid)
	if err != nil {
		t.Fatalf("read index %s: %v", index, err)
	}
	return valid
}

// Happy path (real PG): with both identities, CREATE INDEX CONCURRENTLY
// succeeds through the action pool, and the observation role itself
// cannot ALTER the table (42501). Without the action pool the same build
// fails on the observation role: the routing, not luck, made it work.
func TestActionPool_CreateIndexConcurrentlyRunsAsActionRole(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	table := f.schema + ".t1"
	split := New(f.obs, config.DefaultConfig(), time.Time{}, nopLog).WithActionPool(f.act)
	sql := "CREATE INDEX CONCURRENTLY ap_ix ON " + table + " (id)"
	decision := ActionPolicyDecision{Decision: PolicyDecisionExecute}
	if err := split.runFindingSQL(ctx, identityFinding(sql, table), decision); err != nil {
		t.Fatalf("CREATE INDEX CONCURRENTLY through the action pool: %v", err)
	}
	if !indexIsValid(t, f, f.schema+".ap_ix") {
		t.Fatal("the action pool's index is missing or invalid")
	}
	_, err := f.obs.Exec(ctx, "ALTER TABLE "+table+" ADD COLUMN extra int")
	requireSQLState(t, "ALTER TABLE as the observation role", err, "42501")

	single := New(f.obs, config.DefaultConfig(), time.Time{}, nopLog)
	sql2 := "CREATE INDEX CONCURRENTLY ap_ix2 ON " + table + " (id)"
	err = single.runFindingSQL(ctx, identityFinding(sql2, table), decision)
	requireSQLState(t, "CREATE INDEX as the observation role", err, "42501")
	if indexIsValid(t, f, f.schema+".ap_ix2") {
		t.Fatal("the observation role built an index it must not be able to build")
	}
}

// victim starts a long query as the application role and returns its
// backend evidence and the query's result channel.
func (f *identityFixture) victim(t *testing.T, appName string) (
	backendSignalEvidence, <-chan error) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = f.app, "app"
	cfg.ConnConfig.RuntimeParams["application_name"] = appName
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("victim pool: %v", err)
	}
	t.Cleanup(p.Close)
	done := make(chan error, 1)
	go func() {
		_, err := p.Exec(context.Background(), "SELECT pg_sleep(60)")
		done <- err
	}()
	var ev backendSignalEvidence
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		err = f.super.QueryRow(context.Background(), `SELECT pid, query_start,
			COALESCE(query_id, 0), application_name, LEFT(query, 200), backend_start
			FROM pg_stat_activity WHERE application_name = $1 AND state = 'active'`,
			appName).Scan(&ev.PID, &ev.QueryStart, &ev.QueryID, &ev.AppName, &ev.Query,
			&ev.BackendStart)
		if err == nil {
			return ev, done
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("victim session %s never became active: %v", appName, err)
	return ev, done
}

// Boundary: the observation role cannot signal another role's backend
// (42501), so an executor without the action identity cannot either; the
// executor with it cancels the backend through the action pool.
func TestActionPool_BackendSignalNeedsTheActionIdentity(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	ev, done := f.victim(t, "ap_victim_"+f.app)
	var ok bool
	err := f.obs.QueryRow(ctx, "SELECT pg_terminate_backend($1)", ev.PID).Scan(&ok)
	requireSQLState(t, "pg_terminate_backend as the observation role", err, "42501")

	single := New(f.obs, config.DefaultConfig(), time.Time{}, nopLog)
	err = single.signalMatchingBackend(ctx, "cancel", ev)
	requireSQLState(t, "cancel without the action identity", err, "42501")

	split := New(f.obs, config.DefaultConfig(), time.Time{}, nopLog).WithActionPool(f.act)
	if err := split.signalMatchingBackend(ctx, "cancel", ev); err != nil {
		t.Fatalf("cancel through the action pool: %v", err)
	}
	select {
	case err := <-done:
		requireSQLState(t, "the cancelled victim query", err, "57014")
	case <-time.After(20 * time.Second):
		t.Fatal("the victim query was not cancelled")
	}
}

// Concurrency: as many index builds as there are DDL slots and backend
// signals share the action pool without deadlock. (-race is unavailable on this machine: no
// cgo; the shared state here is the pool, which pgx guards.)
func TestActionPool_BuildsAndSignalsShareThePool(t *testing.T) {
	f := newIdentityFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e := New(f.obs, config.DefaultConfig(), time.Time{}, nopLog).WithActionPool(f.act)
	stale := backendSignalEvidence{PID: 1, QueryStart: time.Now(), BackendStart: time.Now(),
		Query: "SELECT 1", AppName: "nobody"}
	var wg sync.WaitGroup
	errs := make(chan error, 9)
	for i := 1; i <= 3; i++ {
		table := fmt.Sprintf("%s.t%d", f.schema, i)
		sql := fmt.Sprintf("CREATE INDEX CONCURRENTLY ap_cx%d ON %s (id)", i, table)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.runFindingSQL(ctx, identityFinding(sql, table),
				ActionPolicyDecision{Decision: PolicyDecisionExecute})
		}()
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.signalMatchingBackend(ctx, "terminate", stale)
			if !errors.Is(err, ErrBackendEvidenceStale) {
				errs <- fmt.Errorf("stale signal = %w, want ErrBackendEvidenceStale", err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("shared action pool: %v", err)
		}
	}
	if ctx.Err() != nil {
		t.Fatal("the action pool work did not finish before the deadline")
	}
	for i := 1; i <= 3; i++ {
		if !indexIsValid(t, f, fmt.Sprintf("%s.ap_cx%d", f.schema, i)) {
			t.Fatalf("index ap_cx%d is missing or invalid", i)
		}
	}
	if got := f.act.Stat().MaxConns(); got != ActionPoolMaxConns() {
		t.Fatalf("action pool MaxConns = %d, want %d", got, ActionPoolMaxConns())
	}
}

// Integration, PostgreSQL 17+: an action role holding only pg_maintain
// (no owner membership) passes the maintenance precheck and VACUUMs the
// table, because the precheck reads the role that will run the statement;
// an executor without it is withheld on the observation role, which has
// neither.
func TestActionPool_MaintainRoleVacuumsOn17(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	var version int
	if err := f.super.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	if version < 170000 {
		t.Skip("pg_maintain exists from PostgreSQL 17; before it only the owner may VACUUM")
	}
	maint := "ap_mnt" + f.app[len("ap_app"):]
	// USAGE: without owning the schema, the role must still resolve the name.
	for _, s := range []string{"CREATE ROLE " + maint + " LOGIN PASSWORD 'mnt' IN ROLE " +
		"pg_maintain, pg_read_all_stats", "GRANT USAGE ON SCHEMA " + f.schema + " TO " + maint} {
		if _, err := f.super.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = f.super.Exec(context.Background(), `SELECT pg_terminate_backend(pid)
			FROM pg_stat_activity WHERE usename = $1`, maint)
		_, _ = f.super.Exec(context.Background(), "DROP ROLE IF EXISTS "+maint)
	})
	sql := "VACUUM (ANALYZE) " + f.schema + ".t2"
	decision := ActionPolicyDecision{Decision: PolicyDecisionExecute, RiskTier: "safe"}
	single := New(f.obs, config.DefaultConfig(), time.Time{}, nopLog)
	if err := single.admitMaintenance(ctx, sql, decision); !errors.Is(err, ErrActionWithheld) {
		t.Fatalf("precheck on the observation role = %v, want withheld", err)
	}
	split := New(f.obs, config.DefaultConfig(), time.Time{}, nopLog).
		WithActionPool(f.login(t, maint, "mnt"))
	if err := split.admitMaintenance(ctx, sql, decision); err != nil {
		t.Fatalf("precheck on the pg_maintain action role = %v, want admitted", err)
	}
	if err := split.runFindingSQL(ctx, identityFinding(sql, f.schema+".t2"),
		decision); err != nil {
		t.Fatalf("VACUUM as the pg_maintain action role: %v", err)
	}
	var analyzed bool
	if err := f.super.QueryRow(ctx, `SELECT last_vacuum IS NOT NULL FROM
		pg_stat_user_tables WHERE relid = to_regclass($1)`, f.schema+".t2").
		Scan(&analyzed); err != nil || !analyzed {
		t.Fatalf("t2 last_vacuum set = %t (%v), want true", analyzed, err)
	}
}
