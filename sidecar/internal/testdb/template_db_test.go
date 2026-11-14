package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ci-15 integration: the per-run fixture template on the designated test
// server (SAGE_TEST_DATABASE_URL). Run on PG17 and PG14.

func templateAdmin(t *testing.T) (string, *pgx.Conn) {
	t.Helper()
	base := SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return base, conn
}

// scratchTemplate is a template name no other test uses, dropped at the
// end.
func scratchTemplate(t *testing.T, admin *pgx.Conn) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := templatePrefix + hex.EncodeToString(b)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), fixtureDatabaseTimeout)
		defer cancel()
		if err := dropTemplate(ctx, admin, name); err != nil {
			t.Errorf("drop scratch template: %v", err)
		}
	})
	return name
}

type templateRow struct {
	exists, isTemplate, allowConn bool
	sessions                      int
}

func readTemplate(t *testing.T, admin *pgx.Conn, name string) templateRow {
	t.Helper()
	var r templateRow
	err := admin.QueryRow(context.Background(), `SELECT
		EXISTS (SELECT 1 FROM pg_database WHERE datname = $1),
		COALESCE((SELECT datistemplate FROM pg_database WHERE datname = $1), false),
		COALESCE((SELECT datallowconn FROM pg_database WHERE datname = $1), false),
		(SELECT count(*) FROM pg_stat_activity WHERE datname = $1)`, name).Scan(
		&r.exists, &r.isTemplate, &r.allowConn, &r.sessions)
	if err != nil {
		t.Fatalf("read template %s: %v", name, err)
	}
	return r
}

// tableBootstrap creates a marker table: a small stand-in for schema
// bootstrap that counts its runs.
func tableBootstrap(runs *atomic.Int32, hold time.Duration) BootstrapFunc {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		runs.Add(1)
		time.Sleep(hold)
		_, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS sage;
			CREATE TABLE IF NOT EXISTS sage.schema_migrations (id text PRIMARY KEY);
			INSERT INTO sage.schema_migrations VALUES ('marker') ON CONFLICT DO NOTHING`)
		return err
	}
}

func templateCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), templateBuildTimeout)
	t.Cleanup(cancel)
	return ctx
}

// The builder leaves a template no session holds and nobody can connect
// to, so CREATE DATABASE ... TEMPLATE succeeds, and a fixture copied from
// it holds what the bootstrap wrote.
func TestEnsureTemplate_BuildsAClosedTemplate(t *testing.T) {
	base, admin := templateAdmin(t)
	name := scratchTemplate(t, admin)
	var runs atomic.Int32
	if err := ensureTemplate(templateCtx(t), base, name, tableBootstrap(&runs, 0)); err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	got := readTemplate(t, admin, name)
	if !got.exists || !got.isTemplate || got.allowConn || got.sessions != 0 || runs.Load() != 1 {
		t.Fatalf("template %s = %+v after %d runs, want a closed template built once",
			name, got, runs.Load())
	}
	fixture, err := extraDatabaseName("tplcopy")
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := createFixtureFrom(base, fixture, "designated", name)
	if err != nil {
		t.Fatalf("create fixture from %s: %v", name, err)
	}
	t.Cleanup(func() { _ = dropFixture(base, fixture) })
	if n := countMigrations(t, dsn); n != 1 {
		t.Fatalf("fixture copied from the template has %d ledger rows, want 1", n)
	}
}

func countMigrations(t *testing.T, dsn string) int {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect fixture: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var present bool
	if err := conn.QueryRow(context.Background(),
		"SELECT to_regclass('sage.schema_migrations') IS NOT NULL").Scan(&present); err != nil {
		t.Fatalf("look for sage.schema_migrations: %v", err)
	}
	if !present {
		return 0
	}
	var n int
	if err := conn.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count sage.schema_migrations: %v", err)
	}
	return n
}

// Error propagation: a failing bootstrap fails every caller with its error
// and the template name, and leaves no half-built database behind.
func TestEnsureTemplate_BootstrapFailureNamesTheTemplate(t *testing.T) {
	base, admin := templateAdmin(t)
	name := scratchTemplate(t, admin)
	boom := errors.New(`relation "sage.findings" does not exist`)
	failing := func(context.Context, *pgxpool.Pool) error { return boom }
	for i := range 2 {
		err := ensureTemplate(templateCtx(t), base, name, failing)
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), name) ||
			errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller %d: err = %v, want the bootstrap error naming %s", i, err, name)
		}
		if got := readTemplate(t, admin, name); got.exists {
			t.Fatalf("caller %d left %s behind: %+v", i, name, got)
		}
	}
}

// State transition: a database left half-built (not marked as a template)
// by a builder that died is rebuilt, not reused.
func TestEnsureTemplate_RebuildsAHalfBuiltDatabase(t *testing.T) {
	base, admin := templateAdmin(t)
	name := scratchTemplate(t, admin)
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+
		pgx.Identifier{name}.Sanitize()+" TEMPLATE template0"); err != nil {
		t.Fatalf("create half-built database: %v", err)
	}
	var runs atomic.Int32
	if err := ensureTemplate(templateCtx(t), base, name, tableBootstrap(&runs, 0)); err != nil {
		t.Fatalf("ensureTemplate: %v", err)
	}
	if got := readTemplate(t, admin, name); !got.isTemplate || runs.Load() != 1 {
		t.Fatalf("half-built %s = %+v after %d runs, want it rebuilt", name, got, runs.Load())
	}
}

// Boundary and concurrency: callers that start together build the
// template once; the rest wait on the lock and reuse it.
func TestEnsureTemplate_ConcurrentCallersBuildOnce(t *testing.T) {
	for _, callers := range []int{2, 20} {
		t.Run(fmt.Sprintf("%d callers", callers), func(t *testing.T) {
			base, admin := templateAdmin(t)
			name := scratchTemplate(t, admin)
			var runs atomic.Int32
			boot := tableBootstrap(&runs, 300*time.Millisecond)
			ctx := templateCtx(t)
			errs := make([]error, callers)
			var wg sync.WaitGroup
			for i := range callers {
				wg.Go(func() { errs[i] = ensureTemplate(ctx, base, name, boot) })
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("caller %d: %v", i, err)
				}
			}
			var n int
			if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_database
				WHERE datname = $1 AND datistemplate`, name).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if runs.Load() != 1 || n != 1 {
				t.Fatalf("%d callers: %d builds, %d templates; want 1 and 1",
					callers, runs.Load(), n)
			}
		})
	}
}

// State transition: a new checksum makes a new template and leaves the old
// one; the sweep drops templates of other hashes built before its cutoff
// and keeps the current one and recent ones.
func TestSweepTemplates_DropsOnlyOldOtherHashes(t *testing.T) {
	base, admin := templateAdmin(t)
	old, current, recent := scratchTemplate(t, admin), scratchTemplate(t, admin),
		scratchTemplate(t, admin)
	var runs atomic.Int32
	for _, name := range []string{old, current, recent} {
		if err := ensureTemplate(templateCtx(t), base, name, tableBootstrap(&runs, 0)); err != nil {
			t.Fatalf("ensureTemplate %s: %v", name, err)
		}
	}
	if !readTemplate(t, admin, old).exists {
		t.Fatalf("building %s dropped the older template %s", current, old)
	}
	ctx := templateCtx(t)
	stamp := func(name string, at time.Time) {
		t.Helper()
		if err := stampTemplate(ctx, admin, name, at); err != nil {
			t.Fatalf("stamp %s: %v", name, err)
		}
	}
	stamp(old, time.Now().Add(-2*templateSweepAge))
	dropped, err := sweepTemplates(ctx, admin, current, time.Now().Add(-templateSweepAge))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != old {
		t.Fatalf("sweep dropped %v, want only %s", dropped, old)
	}
	if readTemplate(t, admin, old).exists || !readTemplate(t, admin, current).exists ||
		!readTemplate(t, admin, recent).exists {
		t.Fatalf("after the sweep: old %+v current %+v recent %+v",
			readTemplate(t, admin, old), readTemplate(t, admin, current),
			readTemplate(t, admin, recent))
	}
}

// Invalid input reaches no server: a bad template name is refused before
// the lock is taken.
func TestEnsureTemplate_RefusesABadName(t *testing.T) {
	err := ensureTemplate(context.Background(), "postgres://u@127.0.0.1:1/postgres",
		"template1", nil)
	if err == nil || !strings.Contains(err.Error(), "refusing non-template database name") {
		t.Fatalf("ensureTemplate(template1) = %v, want a refusal", err)
	}
}

// Happy path through RunBootstrapped's core: the package's fixture is a
// copy of the template, FixtureTemplate names it, and the package sees the
// ledger already populated.
func TestRunFixtureWith_FixtureIsACopyOfTheTemplate(t *testing.T) {
	_, admin := templateAdmin(t)
	root := fakeRoot(t, "step "+hex.EncodeToString([]byte(t.Name()))+"\n", "package schema")
	hash, err := templateHash(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dropTemplate(context.Background(), admin, templatePrefix+hash) })
	saved := FixtureTemplate()
	t.Cleanup(func() { setFixtureTemplate(saved) })
	var runs atomic.Int32
	var seen int
	var template string
	code := runFixtureWith(func() int {
		seen = countMigrations(t, os.Getenv(EnvName))
		template = FixtureTemplate()
		return 0
	}, "internal/testdb_tpl", root, tableBootstrap(&runs, 0))
	if code != 0 || seen != 1 || template != templatePrefix+hash || runs.Load() != 1 {
		t.Fatalf("code %d, %d ledger rows, template %q, %d builds; want 0, 1, %s, 1",
			code, seen, template, runs.Load(), templatePrefix+hash)
	}
}

// Error propagation through RunBootstrapped's core: the package fails
// before its tests run, naming the template and the bootstrap error.
func TestRunFixtureWith_BootstrapFailureFailsThePackage(t *testing.T) {
	logs := captureDiag(t)
	root := fakeRoot(t, "step "+hex.EncodeToString([]byte(t.Name()))+"\n", "package schema")
	hash, _ := templateHash(root)
	_ = SkipUnlessLive(t)
	saved := FixtureTemplate()
	t.Cleanup(func() { setFixtureTemplate(saved) })
	ran := false
	code := runFixtureWith(func() int { ran = true; return 0 }, "internal/testdb_tpl", root,
		func(context.Context, *pgxpool.Pool) error { return errors.New("bootstrap exploded") })
	if code != 1 || ran {
		t.Fatalf("code %d, tests ran %v; want 1 and no run", code, ran)
	}
	for _, want := range []string{templatePrefix + hash, "bootstrap exploded"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("stderr %q does not name %q", logs.String(), want)
		}
	}
}
