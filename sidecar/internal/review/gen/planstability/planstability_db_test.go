package planstability

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/review/gen/planstability"))
}

func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func execAll(t *testing.T, ctx context.Context, pool *pgxpool.Pool, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// clerkFixture is public.r02c_users: 1,000,000 rows, 4 non-null
// deleted_at, a 50%-NULL status, analyzed at the default target.
func clerkFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	execAll(t, ctx, pool, "DROP TABLE IF EXISTS public.r02c_users, public.r02c_never",
		`CREATE TABLE public.r02c_users (id bigint, deleted_at timestamptz, status text)`,
		`INSERT INTO public.r02c_users SELECT g,
			CASE WHEN g % 250000 = 0 THEN now() END,
			CASE WHEN g % 2 = 0 THEN 'live' END
		   FROM generate_series(1, 1000000) g`,
		"ANALYZE public.r02c_users",
		"CREATE TABLE public.r02c_never (id bigint, k int)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DROP TABLE IF EXISTS public.r02c_users, public.r02c_never")
	})
}

func clerkLiveQuery(text string) query {
	return query{qid: "301", rels: []string{"public.r02c_users"}, execMs: 90_000,
		calls: 1000, rows: 4, text: text}
}

// CHECK-19 on a real server: the Clerk-shape column is flagged through the
// catalog, the 50%-NULL control column on the same table is not, and the
// proposed step runs under the executor's contract.
func TestDB_ClerkShapeFlaggedControlNot(t *testing.T) {
	pool, ctx := livePool(t)
	clerkFixture(t, ctx, pool)
	src := NewDBSource(catalogread.New(pool, catalogread.Default()))
	q := clerkLiveQuery("SELECT id FROM public.r02c_users WHERE deleted_at IS NOT NULL " +
		"AND status = $1")
	cands, cov := generate(t, src, packOf(t, []query{q},
		map[string][]string{"public.r02c_users": {"deleted_at", "status"}}, nil))
	steps := byAction(cands, executor.ActionTypeSetColumnStatistics)
	if len(steps) != 1 || steps[0].Target != "public.r02c_users.deleted_at" {
		t.Fatalf("steps = %+v (coverage %+v), want deleted_at only", cands, cov)
	}
	if steps[0].ForwardSQL !=
		"ALTER TABLE public.r02c_users ALTER COLUMN deleted_at SET STATISTICS 1000" {
		t.Fatalf("forward = %q", steps[0].ForwardSQL)
	}
	for _, c := range cands {
		if strings.Contains(c.Target, "status") {
			t.Fatalf("control column flagged: %+v", c)
		}
	}
	if err := executor.ValidateExecutorSQL(steps[0].ForwardSQL); err != nil {
		t.Fatalf("the executor refuses the step: %v", err)
	}
}

func TestDB_SourceStatesTablesAndTargets(t *testing.T) {
	pool, ctx := livePool(t)
	clerkFixture(t, ctx, pool)
	execAll(t, ctx, pool, "ALTER TABLE public.r02c_users ALTER COLUMN status "+
		"SET STATISTICS 300")
	src := NewDBSource(catalogread.New(pool, catalogread.Default()))
	facts, err := src.Read(ctx, []string{"public.r02c_users", "public.r02c_never",
		"public.r02c_gone"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if facts.DefaultTarget != 100 || facts.Role == "" {
		t.Fatalf("default target %d role %q, want 100 and the role", facts.DefaultTarget,
			facts.Role)
	}
	users := facts.Tables["public.r02c_users"]
	if users.Status != StatusOK || !users.Owned || users.RelTuples < 900_000 {
		t.Fatalf("users = %+v, want ok, owned, about 1M rows", users)
	}
	dc := users.Columns["deleted_at"]
	if !dc.HasStats || dc.NullFrac < 0.999 || dc.StatTarget != nil && *dc.StatTarget != -1 {
		t.Fatalf("deleted_at = %+v, want analyzed at the default target", dc)
	}
	if st := users.Columns["status"]; st.StatTarget == nil || *st.StatTarget != 300 ||
		st.NullFrac < 0.4 || st.NullFrac > 0.6 || st.TopFreq == nil {
		t.Fatalf("status = %+v, want target 300, about half NULL, a top frequency", st)
	}
	if s := facts.Tables["public.r02c_never"].Status; s != StatusNeverAnalyzed {
		t.Fatalf("never analyzed table status = %q", s)
	}
	if s, ok := facts.Tables["public.r02c_gone"]; ok {
		t.Fatalf("a missing table is listed: %+v", s)
	}
}

// Permission denied: a role without SELECT on the table reads no
// pg_stats rows; the table is no_privilege, never "no risk".
func TestDB_NoPrivilegeIsUnknownWithGrant(t *testing.T) {
	pool, ctx := livePool(t)
	clerkFixture(t, ctx, pool)
	execAll(t, ctx, pool, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'r02c_nopriv') THEN
			CREATE ROLE r02c_nopriv NOLOGIN; END IF; END $$`)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET ROLE r02c_nopriv"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "RESET ROLE") }()
	src := NewDBSource(catalogread.New(conn, catalogread.Default()))
	q := clerkLiveQuery("SELECT id FROM public.r02c_users WHERE deleted_at IS NOT NULL")
	cands, cov := generate(t, src, packOf(t, []query{q},
		map[string][]string{"public.r02c_users": {"deleted_at"}}, nil))
	if len(cands) != 0 {
		t.Fatalf("no privilege gave %+v", cands)
	}
	assertUnknown(t, cov, "public.r02c_users",
		"GRANT SELECT ON TABLE public.r02c_users TO r02c_nopriv")
}
