package sagepool_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/sagepool"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
)

// Live tests (integration): each runs on a fresh testdb.CreateDatabase and
// reads pg_stat_statements only for that database (dbid) and its own fresh
// tables, so the shared server's other traffic cannot stand in for it; no
// test here resets or counts cluster-wide state, so none needs Exclusive.
// sagepool_test.go covers the other categories without a database: happy
// path, invalid input, nil/empty, error propagation, boundaries,
// concurrent frontends and state transitions (Configure twice).

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/sagepool"))
}

// lakebaseEnv sets SAGE_LAKEBASE_ENDPOINT and SAGE_LAKEBASE_ENDPOINTS for
// t, and clears PGSSLROOTCERT: sslrootcert=system turns any DSN's sslmode
// into verify-full, which would send a unit test on to Databricks.
func lakebaseEnv(t *testing.T, one, many string) {
	t.Helper()
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", one)
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", many)
	t.Setenv("PGSSLROOTCERT", "")
	// A regression past the TLS refusal must not reach a real workspace
	// through the developer's own Databricks credentials.
	t.Setenv("DATABRICKS_CONFIG_FILE", t.TempDir()+"/absent.cfg")
	t.Setenv("DATABRICKS_CONFIG_PROFILE", "")
	t.Setenv("DATABRICKS_HOST", "https://127.0.0.1:1")
	t.Setenv("DATABRICKS_AUTH_TYPE", "pat")
	t.Setenv("DATABRICKS_TOKEN", "test-not-a-real-token")
}

// lakebaseDSN is a DSN with password secret on host, with sslmode.
func lakebaseDSN(host, sslmode string) string {
	return "postgres://sage:" + secret + "@" + host + ":5432/db?sslmode=" + sslmode
}

// ordinaryPostgres clears the Lakebase environment: every DSN is plain
// PostgreSQL and nothing calls Databricks.
func ordinaryPostgres(t *testing.T) {
	t.Helper()
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
}

// parse is sagepool.ParseConfig, failing t on an error or on a config it
// already made pg_sage's own: ParseConfig only parses, so a caller can set
// more (a hook, a pool size) before Configure.
func parse(t *testing.T, dsn string) *pgxpool.Config {
	t.Helper()
	cfg, err := sagepool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("sagepool.ParseConfig: %v", err)
	}
	if cc := cfg.ConnConfig; cc.AfterConnect != nil ||
		cc.RuntimeParams["application_name"] == selfmonitor.ApplicationName {
		t.Fatal("sagepool.ParseConfig configured the sessions: it must only parse")
	}
	return cfg
}

// liveDB creates a fresh database and returns its DSN, a plain admin
// connection on it (setup and pg_stat_statements reads) and a context
// bounding the test.
func liveDB(t *testing.T, label string) (string, *pgx.Conn, context.Context) {
	t.Helper()
	ordinaryPostgres(t)
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect the fresh database: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	return dsn, admin, ctx
}

func newPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := sagepool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("sagepool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// configuredPool opens a pool through Configure, with hook (if any) set
// as an AfterConnect hook before it and maxConns set after it.
func configuredPool(t *testing.T, ctx context.Context, dsn string, maxConns int32,
	hook pgconn.AfterConnectFunc) *pgxpool.Pool {
	t.Helper()
	cfg := parse(t, dsn)
	cfg.ConnConfig.AfterConnect = hook
	if err := sagepool.Configure(ctx, cfg); err != nil {
		t.Fatalf("sagepool.Configure: %v", err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open the configured pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// hostileDefaults makes SERIALIZABLE and search_path = public the
// database's defaults, as an operator's ALTER DATABASE would, and proves
// a plain session gets them, so the pins have something to override.
func hostileDefaults(t *testing.T, ctx context.Context, admin *pgx.Conn, dsn string) {
	t.Helper()
	var db string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("read the database name: %v", err)
	}
	id := pgx.Identifier{db}.Sanitize()
	for _, sql := range []string{
		"ALTER DATABASE " + id + " SET default_transaction_isolation = 'serializable'",
		"ALTER DATABASE " + id + " SET search_path = public",
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	plain, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("plain session: %v", err)
	}
	defer func() { _ = plain.Close(context.Background()) }()
	var iso, path string
	if err := plain.QueryRow(ctx, "SELECT current_setting('transaction_isolation'), "+
		"current_setting('search_path')").Scan(&iso, &path); err != nil {
		t.Fatalf("read the plain session's settings: %v", err)
	}
	if iso != "serializable" || path != "public" {
		t.Fatalf("a plain session got %q, %q: the database defaults did not apply", iso, path)
	}
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// sessionSQL reads what the pins and the tagger decide; current_query()
// is the text the server received, so it shows where the tag landed.
const sessionSQL = "SELECT current_setting('application_name'), " +
	"current_setting('search_path'), current_setting('transaction_isolation'), " +
	"current_setting('default_transaction_isolation'), current_query(), pg_backend_pid()"

// taggedSessionSQL is sessionSQL with the tag after its first keyword.
var taggedSessionSQL = "SELECT " + selfmonitor.StatementTag +
	strings.TrimPrefix(sessionSQL, "SELECT")

type sessionState struct {
	app, path, iso, defaultISO, query string
	pid                               int32
	err                               error
}

func readSession(ctx context.Context, q querier) sessionState {
	var s sessionState
	s.err = q.QueryRow(ctx, sessionSQL).Scan(&s.app, &s.path, &s.iso, &s.defaultISO,
		&s.query, &s.pid)
	return s
}

// problems lists how s is not a pinned, tagged pg_sage session.
func (s sessionState) problems() []string {
	if s.err != nil {
		return []string{"read the session: " + s.err.Error()}
	}
	var out []string
	for _, c := range []struct{ what, got, want string }{
		{"application_name", s.app, selfmonitor.ApplicationName},
		{"search_path", s.path, testdb.PinnedPath},
		{"transaction_isolation", s.iso, "read committed"},
		{"default_transaction_isolation", s.defaultISO, "read committed"},
		{"current_query()", s.query, taggedSessionSQL},
	} {
		if c.got != c.want {
			out = append(out, fmt.Sprintf("%s = %q, want %q", c.what, c.got, c.want))
		}
	}
	return out
}

func TestNewLive_SessionIsPgSagesOwnUnderHostileDatabaseDefaults(t *testing.T) {
	dsn, admin, ctx := liveDB(t, "sagepool session")
	hostileDefaults(t, ctx, admin, dsn)
	pool := newPool(t, ctx, dsn)
	for _, p := range readSession(ctx, pool).problems() {
		t.Error(p)
	}
	// A BEGIN without a level takes the pinned default, not the database's.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var iso string
	err = tx.QueryRow(ctx, "SELECT current_setting('transaction_isolation')").Scan(&iso)
	if err != nil || iso != "read committed" {
		t.Errorf("transaction isolation in BEGIN = %q (%v), want read committed", iso, err)
	}
}

// State transition: an AfterConnect hook set before Configure runs first
// (its marker is on the session) and cannot undo the pins (they run
// after it). Its multi-statement SET goes out through the tagger too.
func TestConfigureLive_HookAlreadySetRunsBeforeThePins(t *testing.T) {
	dsn, _, ctx := liveDB(t, "sagepool hook")
	hook := func(ctx context.Context, pc *pgconn.PgConn) error {
		return pc.Exec(ctx, "SET SESSION search_path = public; "+
			"SET SESSION default_transaction_isolation = 'serializable'; "+
			"SET SESSION sagepool_test.hook = 'ran'").Close()
	}
	pool := configuredPool(t, ctx, dsn, 1, hook)
	for _, p := range readSession(ctx, pool).problems() {
		t.Error(p)
	}
	var marker string
	if err := pool.QueryRow(ctx, "SELECT COALESCE(current_setting('sagepool_test.hook', "+
		"true), '')").Scan(&marker); err != nil {
		t.Fatalf("read the hook's marker: %v", err)
	}
	if marker != "ran" {
		t.Errorf("sagepool_test.hook = %q, want ran: the earlier hook was dropped", marker)
	}
}

// The pool connects lazily: a database that does not exist surfaces at
// the first use as the server's own error, not at New.
func TestNewLive_MissingDatabaseSurfacesAtFirstUse(t *testing.T) {
	ordinaryPostgres(t)
	u, err := url.Parse(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse the test DSN: %v", err)
	}
	u.Path = "/pgsage_sagepool_no_such_db_" + strings.ToLower(rand.Text()[:8])
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := newPool(t, ctx, u.String())
	err = pool.Ping(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "3D000" {
		t.Fatalf("Ping on a missing database = %v, want SQLSTATE 3D000 (invalid_catalog_name)",
			err)
	}
}

// probe is one statement form sent through a sagepool, on a fresh table
// of its own (a new relation is a new queryid, so no earlier text of the
// same statement can stand in), and the text pg_stat_statements must hold.
type probe struct {
	form, table, sql, want string
	args                   []any
}

func probes(t *testing.T, ctx context.Context, admin *pgx.Conn) []probe {
	t.Helper()
	suffix := strings.ToLower(rand.Text()[:10])
	tag := selfmonitor.StatementTag
	var out []probe
	for _, form := range []string{"plain", "leading", "simple"} {
		table := "public.sagepool_" + form + "_" + suffix
		if _, err := admin.Exec(ctx, "CREATE TABLE "+table+" (id int)"); err != nil {
			t.Fatalf("create %s: %v", table, err)
		}
		p := probe{form: form, table: table, args: []any{1},
			sql:  "SELECT id FROM " + table + " WHERE id = $1",
			want: "SELECT " + tag + " id FROM " + table + " WHERE id = $1"}
		switch form {
		case "leading": // written with the tag in front, as pg_sage's source does
			p.sql = tag + " " + p.sql
		case "simple": // no arguments: pgx sends a simple Query message
			p.args = nil
			p.sql = "SELECT count(*) FROM " + table
			p.want = "SELECT " + tag + " count(*) FROM " + table
		}
		out = append(out, p)
	}
	return out
}

// The tagger moves a leading tag after the first keyword on every server
// version, so the stored text is the same on 14 through 18; on 18 it is
// the only place pg_stat_statements keeps the tag (it drops a comment
// before the first token).
func TestNewLive_MarkedStatementsLandInPgStatStatementsTagged(t *testing.T) {
	dsn, admin, ctx := liveDB(t, "sagepool pgss")
	var version int
	if err := admin.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	ps := probes(t, ctx, admin)
	pool := newPool(t, ctx, dsn)
	pgssepoch.Attempt(t, ctx, admin, 3, func() []string {
		for _, p := range ps {
			if _, err := pool.Exec(ctx, p.sql, p.args...); err != nil {
				t.Fatalf("%s probe: %v", p.form, err)
			}
		}
		var problems []string
		for _, p := range ps {
			texts := storedTexts(t, ctx, admin, p.table)
			if len(texts) != 1 || texts[0] != p.want {
				problems = append(problems, fmt.Sprintf("%s: server %d stored %q, want "+
					"exactly [%q]", p.form, version, texts, p.want))
			}
		}
		return problems
	})
}

// storedTexts returns this database's pg_stat_statements texts naming
// table, apart from the fixture's own CREATE TABLE.
func storedTexts(t *testing.T, ctx context.Context, admin *pgx.Conn, table string) []string {
	t.Helper()
	rel, err := extschema.StatementsRel(ctx, admin, "pg_stat_statements")
	if err != nil {
		t.Fatalf("find pg_stat_statements: %v", err)
	}
	rows, err := admin.Query(ctx, `SELECT query FROM `+rel+`
		WHERE dbid = (SELECT oid FROM pg_catalog.pg_database
		              WHERE datname = pg_catalog.current_database())
		  AND strpos(query, $1) > 0`, table)
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan pg_stat_statements: %v", err)
	}
	out := texts[:0]
	for _, text := range texts {
		if !strings.HasPrefix(text, "CREATE TABLE") {
			out = append(out, text)
		}
	}
	return out
}

// holdSessions has n goroutines each acquire a connection, read it, and
// hold it until all have read, so n sessions open at once.
func holdSessions(ctx context.Context, pool *pgxpool.Pool, n int) []sessionState {
	states := make([]sessionState, n)
	var all, done sync.WaitGroup
	all.Add(n)
	for i := range n {
		done.Go(func() {
			conn, err := pool.Acquire(ctx)
			if err != nil {
				states[i].err = err
				all.Done()
				return
			}
			defer conn.Release()
			states[i] = readSession(ctx, conn)
			all.Done()
			all.Wait()
		})
	}
	done.Wait()
	return states
}

// Sessions the pool opens concurrently are each pinned and tagged, and
// many more goroutines than sessions sharing them all see the pins.
func TestConfigureLive_ConcurrentSessionsAllPinned(t *testing.T) {
	dsn, admin, ctx := liveDB(t, "sagepool concurrent")
	hostileDefaults(t, ctx, admin, dsn)
	const sessions = 8
	pool := configuredPool(t, ctx, dsn, sessions, nil)
	pids := map[int32]bool{}
	for i, s := range holdSessions(ctx, pool, sessions) {
		for _, p := range s.problems() {
			t.Errorf("session %d: %s", i, p)
		}
		pids[s.pid] = true
	}
	if len(pids) != sessions {
		t.Errorf("%d distinct backends, want %d held at once", len(pids), sessions)
	}
	const readers = 64
	shared := make([]sessionState, readers)
	var wg sync.WaitGroup
	for i := range readers {
		wg.Go(func() { shared[i] = readSession(ctx, pool) })
	}
	wg.Wait()
	for i, s := range shared {
		for _, p := range s.problems() {
			t.Errorf("reader %d: %s", i, p)
		}
	}
}

// On a database whose path resolves planted public functions and
// operators first, the unqualified names a sagepool session runs resolve
// pg_catalog's: no planted function is ever called.
func TestNewLive_PlantedPathIsNeverResolved(t *testing.T) {
	ordinaryPostgres(t)
	plant := testdb.PlantedPathDatabase(t, "sagepool planted")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newPool(t, ctx, plant.DSN)
	var lower string
	var texteq, nameeq, nametext, domaineq bool
	err := pool.QueryRow(ctx, `SELECT lower('X'::text), 'a'::text = 'b'::text,
		'a'::name = 'b'::name, 'a'::name = 'b'::text,
		'a'::public.planted_label = 'b'::public.planted_tag`).
		Scan(&lower, &texteq, &nameeq, &nametext, &domaineq)
	if err != nil {
		t.Fatalf("unqualified statement on the planted database: %v", err)
	}
	if lower != "x" || texteq || nameeq || nametext || domaineq {
		t.Errorf("results %q %v %v %v %v, want x and four false", lower, texteq, nameeq,
			nametext, domaineq)
	}
	plant.AssertNoPlantedCalls(t, pool)
}
