//go:build cgo

package broker

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/agentguard/broker"))
}

// fixture is one test's world: a schema of its own, a broker login role
// named as the core names it, and a broker over the real classification
// store and audit table of the package's fixture database.
type fixture struct {
	super   *pgxpool.Pool
	schema  string
	p       agentguard.Principal
	role    string
	pw      string
	logins  *fakeLogins
	decider *fakeDecider
	target  Target
	broker  *Broker
	cfg     Config
}

const fixtureTables = `
CREATE TABLE %[1]s.items (id int PRIMARY KEY, name text NOT NULL);
INSERT INTO %[1]s.items SELECT g, 'item-' || g FROM generate_series(1, 5) g;
CREATE TABLE %[1]s.people (id int PRIMARY KEY, name text NOT NULL, ssn text NOT NULL,
	api_key text NOT NULL DEFAULT 'k-secret');
INSERT INTO %[1]s.people VALUES (1, 'ann', '123-45-6789'), (2, 'bob', '987-65-4321');
CREATE TABLE %[1]s.hidden (id int);
CREATE TABLE %[1]s.owned (id int, owner_name text);
INSERT INTO %[1]s.owned VALUES (1, 'nobody'), (2, '%[2]s');
ALTER TABLE %[1]s.owned ENABLE ROW LEVEL SECURITY;
CREATE POLICY mine ON %[1]s.owned USING (owner_name = current_user);
CREATE VIEW %[1]s.people_view AS SELECT id, ssn FROM %[1]s.people;
`

func randomPrincipalID(t *testing.T) string {
	t.Helper()
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, 20)
	_, err := rand.Read(b)
	require.NoError(t, err)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "agp_" + string(b)
}

func livePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, schema.Bootstrap(ctx, pool))
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql)
	require.NoError(t, err)
}

func ident(s string) string { return pgx.Identifier{s}.Sanitize() }

// newFixture builds the schema and the broker role, then a broker that
// evaluates the database as env.
func newFixture(t *testing.T, env envbind.Env, mutate ...func(*Config)) *fixture {
	t.Helper()
	return newFixtureOn(t, livePool(t), env, mutate...)
}

// newFixtureOn builds the fixture on super, a bootstrapped superuser pool.
func newFixtureOn(t *testing.T, super *pgxpool.Pool, env envbind.Env,
	mutate ...func(*Config)) *fixture {
	t.Helper()
	// The broker role is a cluster-wide agent role: serialize with every
	// test that creates agent roles or assumes none exists.
	release, err := testdb.LockCluster(context.Background(), os.Getenv(testdb.EnvName),
		testdb.AgentRolesLock)
	require.NoError(t, err)
	t.Cleanup(release)
	f := &fixture{super: super, cfg: DefaultConfig()}
	f.p = testPrincipal()
	f.p.ID = randomPrincipalID(t)
	f.role = agentguard.BrokerRoleName(f.p.ID)
	f.schema = "bq_" + strings.TrimPrefix(f.role, "sage_agentb_")
	f.pw = "pw-" + f.p.ID[4:14]
	var db string
	require.NoError(t, super.QueryRow(context.Background(),
		"SELECT current_database()").Scan(&db))
	exec(t, super, "CREATE SCHEMA "+ident(f.schema))
	exec(t, super, fmt.Sprintf(fixtureTables, ident(f.schema), f.role))
	exec(t, super, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' CONNECTION LIMIT 2 "+
		"NOSUPERUSER NOBYPASSRLS", ident(f.role), f.pw))
	exec(t, super, fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", ident(db), ident(f.role)))
	exec(t, super, fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", ident(f.schema),
		ident(f.role)))
	for _, tbl := range []string{"items", "people", "owned", "people_view"} {
		exec(t, super, fmt.Sprintf("GRANT SELECT ON %s.%s TO %s", ident(f.schema), tbl,
			ident(f.role)))
	}
	t.Cleanup(func() { f.teardown(db) })
	for _, m := range mutate {
		m(&f.cfg)
	}
	f.logins = &fakeLogins{role: f.role, pw: f.pw}
	f.decider = allowIn(env, f.p)
	f.target = Target{Name: "db", DatabaseID: "00000000-0000-4000-8000-00000000b001",
		Pool: super, ClusterKey: "test-cluster"}
	f.broker = f.newBroker(t, &SQLAudit{})
	return f
}

func (f *fixture) newBroker(t *testing.T, audit AuditSink) *Broker {
	t.Helper()
	b, err := New(f.cfg, Deps{Targets: fakeTargets{"db": f.target}, Logins: f.logins,
		Decider: f.decider, Classes: StoreClasses{}, Audit: audit})
	require.NoError(t, err)
	t.Cleanup(b.Close)
	return b
}

func (f *fixture) teardown(db string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if f.broker != nil {
		f.broker.Close()
	}
	stmts := []string{
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = '" +
			f.role + "'",
		"DROP SCHEMA IF EXISTS " + ident(f.schema) + " CASCADE",
		"REVOKE ALL ON DATABASE " + ident(db) + " FROM " + ident(f.role),
		"DROP ROLE IF EXISTS " + ident(f.role),
	}
	for _, s := range stmts {
		_, _ = f.super.Exec(ctx, s)
	}
}

func (f *fixture) ctx() context.Context {
	return withAgent(context.Background(), f.p, nil)
}

func (f *fixture) query(t *testing.T, sql string, params ...any) Result {
	t.Helper()
	res, err := f.broker.Query(f.ctx(), Request{Database: "db", SQL: sql, Params: params})
	require.NoError(t, err)
	return res
}

// classifyColumn confirms a class for schema.table.column.
func (f *fixture) classifyColumn(t *testing.T, table, column string, c classify.Class) {
	t.Helper()
	ctx := context.Background()
	store := classify.NewStore(f.super)
	col, err := store.ResolveColumn(ctx, f.schema, table, column)
	require.NoError(t, err)
	_, err = store.Set(ctx, col, c, "test-admin@example.com", "fixture")
	require.NoError(t, err)
}

// auditRows reads this principal's guard_query_audit rows, oldest first.
func (f *fixture) auditRows(t *testing.T) []auditRow {
	t.Helper()
	rows, err := f.super.Query(context.Background(), `SELECT verdict, coalesce(reason, ''),
		coalesce(step, ''), coalesce(row_count, -1), coalesce(classes, '{}'),
		envelope_hash, coalesce(task_id, '') FROM sage.guard_query_audit
		WHERE principal_id = $1 ORDER BY id`, f.p.ID)
	require.NoError(t, err)
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		require.NoError(t, rows.Scan(&r.verdict, &r.reason, &r.step, &r.rowCount, &r.classes,
			&r.envelope, &r.task))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

type auditRow struct {
	verdict, reason, step, envelope, task string
	rowCount                              int
	classes                               []string
}
