package agentposture

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// fixture is one detector test's private corner of the cluster: a schema
// and an exposed role (both unique), dropped when the test ends. The
// exposed role has USAGE on the schema; PUBLIC has none.
type fixture struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	schema  string // unquoted; names here are plain lower case
	exposed string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool, ctx := livePool(t)
	// Agent roles are cluster-wide: AP-13 and AP-16 assume none exists, so
	// posture fixtures serialize with every test that creates agent roles.
	release, err := testdb.LockCluster(ctx, os.Getenv(testdb.EnvName), testdb.AgentRolesLock)
	if err != nil {
		t.Fatalf("agent roles lock: %v", err)
	}
	t.Cleanup(release)
	s := suffix(t)
	f := &fixture{t: t, ctx: ctx, pool: pool, schema: "pst_" + s, exposed: "pexp_" + s}
	createRole(t, ctx, pool, f.exposed, "NOLOGIN")
	execAll(t, ctx, pool, "CREATE SCHEMA "+f.schema,
		"GRANT USAGE ON SCHEMA "+f.schema+" TO "+f.exposed)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+f.schema+" CASCADE")
	})
	return f
}

func (f *fixture) exec(stmts ...string) {
	f.t.Helper()
	execAll(f.t, f.ctx, f.pool, stmts...)
}

// q qualifies name with the fixture schema.
func (f *fixture) q(name string) string { return f.schema + "." + name }

// role creates another role, dropped when the test ends.
func (f *fixture) role(prefix, attrs string) string {
	f.t.Helper()
	name := prefix + suffix(f.t)
	createRole(f.t, f.ctx, f.pool, name, attrs)
	return name
}

// env resolves the environment with the fixture's exposed role and no
// client hints, then lets mut add to it.
func (f *fixture) env(mut func(*Env)) Env {
	f.t.Helper()
	cfg := DefaultConfig()
	cfg.ExposedRoles = []string{f.exposed}
	cfg.ClientPatterns = nil
	var env Env
	var err error
	readTx(f.t, f.ctx, f.pool, func(tx pgx.Tx) { env, err = ResolveEnv(f.ctx, tx, cfg) })
	if err != nil {
		f.t.Fatalf("ResolveEnv: %v", err)
	}
	if mut != nil {
		mut(&env)
	}
	return env
}

// agent adds role name to env as an agent role from source.
func (f *fixture) agent(env *Env, name string, src RoleSource) {
	f.t.Helper()
	var oid uint32
	if err := f.pool.QueryRow(f.ctx, "SELECT oid FROM pg_roles WHERE rolname = $1",
		name).Scan(&oid); err != nil {
		f.t.Fatalf("role %s: %v", name, err)
	}
	env.Agents = append(env.Agents, Role{OID: oid, Name: name, Source: src})
	env.PrincipalsExist = env.PrincipalsExist || src == SourceRegistered
}

// run runs the registered detector id against env in a read-only
// transaction.
func (f *fixture) run(id string, env Env) Outcome {
	f.t.Helper()
	d, ok := Default().Get(id)
	if !ok {
		f.t.Fatalf("%s is not registered", id)
	}
	var out Outcome
	var err error
	readTx(f.t, f.ctx, f.pool, func(tx pgx.Tx) { out, err = RunDetector(f.ctx, d, tx, env) })
	if err != nil {
		f.t.Fatalf("%s: %v", id, err)
	}
	return out
}

func findObject(fs []Finding, object string) *Finding {
	for i := range fs {
		if fs[i].Object == object {
			return &fs[i]
		}
	}
	return nil
}

// requireFinding returns the finding on object and checks what every
// posture finding must carry.
func requireFinding(t *testing.T, o Outcome, object string, sev Severity) Finding {
	t.Helper()
	f := findObject(o.Findings, object)
	if f == nil {
		var objs []string
		for _, x := range o.Findings {
			objs = append(objs, x.Object)
		}
		t.Fatalf("%s: no finding on %s; found %v", o.Detector, object, objs)
	}
	if f.Severity != sev {
		t.Fatalf("%s on %s: severity %s, want %s", o.Detector, object, f.Severity, sev)
	}
	if f.Detector != o.Detector || f.Title == "" || f.FixScript == "" ||
		len(f.Evidence) == 0 || f.Recommendation == "" {
		t.Fatalf("%s on %s is missing title, fix, recommendation or evidence: %+v",
			o.Detector, object, *f)
	}
	return *f
}

func requireNoFinding(t *testing.T, o Outcome, object string) {
	t.Helper()
	if f := findObject(o.Findings, object); f != nil {
		t.Fatalf("%s reported %s, which it must not: %+v", o.Detector, object, *f)
	}
}

func requireContains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Fatalf("%s = %q, want it to contain %q", what, got, w)
		}
	}
}
