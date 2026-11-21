package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The control database a reconcile test runs against: a fresh database
// with the schema, two monitored databases (orders tier=prod, billing
// tier=stage) whose policy sets are bootstrapped, and three users (alice
// and bob viewers, carol an admin).
type fixture struct {
	pool    *pgxpool.Pool
	users   map[string]int
	orders  int64
	billing int64
	// base is the default set's document; narrow and wide lower and raise
	// its lock ceiling.
	base, narrow, wide json.RawMessage
}

var monitored = []string{"orders", "billing"}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "gitops"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	f := &fixture{pool: pool, users: map[string]int{}}
	f.orders = f.addDatabase(t, "orders", "prod")
	f.billing = f.addDatabase(t, "billing", "stage")
	store := policy.NewStore(pool)
	for _, id := range []int64{f.orders, f.billing} {
		if _, err := store.Bootstrap(ctx, policy.Scope{DatabaseID: &id}, "unattended",
			"test"); err != nil {
			t.Fatalf("bootstrap policy: %v", err)
		}
	}
	for _, u := range []struct{ email, role string }{{"alice@example.com", "viewer"},
		{"bob@example.com", "viewer"}, {"carol@example.com", "admin"}} {
		var id int
		if err := pool.QueryRow(ctx, `INSERT INTO sage.users (email, password, role)
			VALUES ($1, 'x', $2) RETURNING id`, u.email, u.role).Scan(&id); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		f.users[u.email] = id
	}
	f.base = f.setDoc(t, "default")
	f.narrow, f.wide = withCeiling(t, f.base, -1), withCeiling(t, f.base, +1)
	return f
}

func (f *fixture) addDatabase(t *testing.T, name, tier string) int64 {
	t.Helper()
	var id int64
	err := f.pool.QueryRow(context.Background(), `
INSERT INTO sage.databases (name, host, database_name, username, password_enc, tags)
VALUES ($1, 'localhost', $1, 'sage', '\x00', jsonb_build_object('tier', $2::text))
RETURNING id`, name, tier).Scan(&id)
	if err != nil {
		t.Fatalf("insert database %s: %v", name, err)
	}
	return id
}

// withCeiling is doc with lock_duration_ceiling_ms halved (-1) or doubled.
func withCeiling(t *testing.T, doc json.RawMessage, dir int) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatal(err)
	}
	ceiling, _ := m["lock_duration_ceiling_ms"].(float64)
	if ceiling <= 1 {
		t.Fatalf("the default document has no lock ceiling to move: %v", m)
	}
	if dir < 0 {
		m["lock_duration_ceiling_ms"] = ceiling / 2
	} else {
		m["lock_duration_ceiling_ms"] = ceiling * 2
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *fixture) setDoc(t *testing.T, set string) json.RawMessage {
	t.Helper()
	var doc json.RawMessage
	err := f.pool.QueryRow(context.Background(), `SELECT doc FROM sage.policy
		WHERE set_name = $1 AND database_id IS NULL AND status = 'active'`, set).Scan(&doc)
	if err != nil {
		t.Fatalf("active document of set %s: %v", set, err)
	}
	return doc
}

func (f *fixture) ceiling(t *testing.T, set string) float64 {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(f.setDoc(t, set), &m); err != nil {
		t.Fatal(err)
	}
	v, _ := m["lock_duration_ceiling_ms"].(float64)
	return v
}

func (f *fixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// rowCounts is every table a reconcile may write, by name.
func (f *fixture) rowCounts(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"policy_sets", "policy", "policy_set_events",
		"guard_principals", "guard_principal_changes", "user_database_grants",
		"auth_audit", "policy_approvals"} {
		out[table] = f.count(t, "SELECT count(*) FROM sage."+table)
	}
	return out
}

func (f *fixture) reconcile(t *testing.T, root, mode string) (Result, error) {
	t.Helper()
	dir, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return Reconcile(context.Background(), f.pool, dir,
		Options{Mode: mode, Databases: monitored, Leader: true})
}

func (f *fixture) apply(t *testing.T, root string) Result {
	t.Helper()
	res, err := f.reconcile(t, root, ModeApply)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return res
}

// setFile is a policy-set file; doc nil leaves the document out.
func setFile(name string, priority int, tier string, doc json.RawMessage) string {
	out := fmt.Sprintf("kind: PolicySet\nschema_version: 1\nname: %s\npriority: %d\n"+
		"selectors:\n  - tags: {tier: %s}\n", name, priority, tier)
	if doc != nil {
		out += "document:\n  profile: unattended\n  body: " + string(doc) + "\n"
	}
	return out
}

func principalFile(name, profile, ceiling string, extra ...string) string {
	return fmt.Sprintf("kind: Principal\nschema_version: 1\nname: %s\nprofile: %s\n"+
		"env_ceiling: %s\n", name, profile, ceiling) + strings.Join(extra, "")
}

func grantFile(grants ...[3]string) string {
	out := "kind: DatabaseGrants\nschema_version: 1\ngrants:\n"
	for _, g := range grants {
		out += fmt.Sprintf("  - user: %s\n    database: %s\n    role: %s\n", g[0], g[1], g[2])
	}
	return out
}

// happyFiles: two sets (one with a narrower document), one principal and
// two grants.
func (f *fixture) happyFiles() map[string]string {
	return map[string]string{
		"policy-sets/prod-strict.yaml": setFile("prod-strict", 50, "prod", f.narrow),
		"policy-sets/stage.yaml":       setFile("stage", 40, "stage", nil),
		"principals/ci-bot.yaml":       principalFile("ci-bot", "readonly-analyst", "dev"),
		"grants/orders.yaml": grantFile([3]string{"alice@example.com", "orders", "viewer"},
			[3]string{"bob@example.com", "orders", "operator"}),
	}
}

func ops(res Result) map[string]int {
	out := map[string]int{}
	for _, c := range res.Changes {
		out[c.Kind+":"+c.Op]++
	}
	return out
}

// apiRows is every API-owned principal, grant and set, with its update
// time, as one string.
func (f *fixture) apiRows(t *testing.T) string {
	t.Helper()
	var out string
	err := f.pool.QueryRow(context.Background(), `SELECT
		(SELECT string_agg(p.name || p.status || p.profile || p.env_ceiling ||
		        p.updated_at::text, ',') FROM sage.guard_principals p
		 WHERE managed_by = 'api') || '|' ||
		(SELECT string_agg(user_id || database_name || role || source ||
		        granted_at::text, ',') FROM sage.user_database_grants
		 WHERE source <> 'config') || '|' ||
		(SELECT string_agg(name || priority || selectors::text || updated_at::text, ',')
		 FROM sage.policy_sets WHERE managed_by = 'api')`).Scan(&out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
