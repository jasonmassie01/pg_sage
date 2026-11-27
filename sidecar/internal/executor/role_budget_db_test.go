package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// market-08 on a live server: ALTER ROLE ... IN DATABASE round-trips, a
// new session sees it, rollback leaves pg_db_role_setting byte-identical,
// refusals happen before any change, and two databases enforcing on one
// role serialize through the cluster lock.

const budgetRole = "budget_exec_app"

func budgetRoleSetup(t *testing.T, p *pgxpool.Pool, extra ...string) string {
	t.Helper()
	ctx := context.Background()
	testdb.LockAgentRoles(t)
	stmts := append([]string{`DROP ROLE IF EXISTS ` + budgetRole,
		`CREATE ROLE ` + budgetRole + ` LOGIN PASSWORD 'budget-test' CONNECTION LIMIT 25`},
		extra...)
	for _, sql := range stmts {
		if _, err := p.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = p.Exec(context.Background(), `DROP OWNED BY `+budgetRole)
		_, _ = p.Exec(context.Background(), `DROP ROLE IF EXISTS `+budgetRole)
	})
	var db string
	if err := p.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	return db
}

// roleSettings is the role's raw pg_db_role_setting rows, every database.
func roleSettings(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	var raw string
	err := p.QueryRow(context.Background(), `SELECT COALESCE(string_agg(
		s.setdatabase::text || ':' || array_to_string(s.setconfig, ','), ';'
		ORDER BY s.setdatabase), '')
		FROM pg_db_role_setting s JOIN pg_roles r ON r.oid = s.setrole
		WHERE r.rolname = $1`, budgetRole).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func roleBudgetExecutor(p *pgxpool.Pool) *Executor {
	return New(p, config.DefaultConfig(), time.Time{}, nopLog)
}

// runRoleBudget prepares and executes sql as the executor does.
func runRoleBudget(t *testing.T, e *Executor, sql string) (*configChange, error) {
	t.Helper()
	ctx := context.Background()
	change, err := e.prepareConfigChange(ctx, sql)
	if err != nil {
		return nil, err
	}
	if change == nil || change.kind != configKindRoleBudget {
		t.Fatalf("prepare(%q) = %+v, want a role budget change", sql, change)
	}
	state := map[string]any{}
	change.record(state)
	return change, e.execRoleBudget(ctx, sql, state, 5*time.Second, 2000)
}

// Integration and state transitions (warn -> enforce -> reverted):
// the role setting round-trips, a session opened afterwards sees the new
// statement_timeout, and rollback restores the prior non-default value
// byte-identically (CHECK-14).
func TestRoleBudget_TimeoutRoundTripAndRollback(t *testing.T) {
	p, ctx := requireDB(t)
	db := budgetRoleSetup(t, p)
	quoted := pgx.Identifier{db}.Sanitize()
	if _, err := p.Exec(ctx, `ALTER ROLE `+budgetRole+` IN DATABASE `+quoted+
		` SET statement_timeout = '45s'`); err != nil {
		t.Fatal(err)
	}
	before := roleSettings(t, p)
	e := roleBudgetExecutor(p)
	sql := `ALTER ROLE "` + budgetRole + `" IN DATABASE ` + quoted +
		` SET statement_timeout = '6000ms'`
	change, err := runRoleBudget(t, e, sql)
	if err != nil {
		t.Fatalf("enforce: %v", err)
	}
	want := `ALTER ROLE "` + budgetRole + `" IN DATABASE ` + quoted +
		` SET statement_timeout = '45s'`
	if change.rollbackSQL != want {
		t.Fatalf("rollback = %q, want the prior 45s restored (not RESET): %q",
			change.rollbackSQL, want)
	}
	state, note, _ := e.readBackConfig(ctx, change)
	if state != readbackInEffect {
		t.Fatalf("read-back = %s (%s)", state, note)
	}
	if got := sessionStatementTimeout(t, p, db); got != "6s" {
		t.Fatalf("a new session of the role sees statement_timeout %q, want 6s", got)
	}
	if _, err := runRoleBudget(t, e, change.rollbackSQL); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if after := roleSettings(t, p); after != before {
		t.Fatalf("pg_db_role_setting after rollback = %q, before = %q", after, before)
	}
	if got := sessionStatementTimeout(t, p, db); got != "45s" {
		t.Fatalf("after rollback a new session sees %q, want 45s", got)
	}
}

// sessionStatementTimeout opens a new session as the budgeted role.
func sessionStatementTimeout(t *testing.T, p *pgxpool.Pool, db string) string {
	t.Helper()
	cfg := p.Config().ConnConfig.Copy()
	cfg.User, cfg.Password, cfg.Database = budgetRole, "budget-test", db
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", budgetRole, err)
	}
	defer conn.Close(context.Background())
	var v string
	if err := conn.QueryRow(context.Background(), "SHOW statement_timeout").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// Rollback of a role with no prior setting is RESET, and leaves no row.
func TestRoleBudget_NoPriorRollsBackToReset(t *testing.T) {
	p, _ := requireDB(t)
	db := budgetRoleSetup(t, p)
	before := roleSettings(t, p)
	e := roleBudgetExecutor(p)
	sql := `ALTER ROLE "` + budgetRole + `" IN DATABASE ` + pgx.Identifier{db}.Sanitize() +
		` SET idle_in_transaction_session_timeout = '30000ms'`
	change, err := runRoleBudget(t, e, sql)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(change.rollbackSQL, "RESET idle_in_transaction_session_timeout") {
		t.Fatalf("rollback = %q, want RESET", change.rollbackSQL)
	}
	if _, err := runRoleBudget(t, e, change.rollbackSQL); err != nil {
		t.Fatal(err)
	}
	if after := roleSettings(t, p); after != before {
		t.Fatalf("after rollback %q, before %q", after, before)
	}
}

// Invalid input: a superuser, pg_sage's own role and another database
// are refused before anything changes; a connection limit below current
// sessions plus a margin is refused; a sound one round-trips.
func TestRoleBudget_Refusals(t *testing.T) {
	p, ctx := requireDB(t)
	db := budgetRoleSetup(t, p)
	e := roleBudgetExecutor(p)
	var self string
	if err := p.QueryRow(ctx, "SELECT session_user").Scan(&self); err != nil {
		t.Fatal(err)
	}
	quoted := pgx.Identifier{db}.Sanitize()
	cases := map[string]string{
		"pg_sage's own role": `ALTER ROLE ` + pgx.Identifier{self}.Sanitize() +
			` IN DATABASE ` + quoted + ` SET statement_timeout = '6000ms'`,
		"connected database": `ALTER ROLE "` + budgetRole +
			`" IN DATABASE "postgres_other" SET statement_timeout = '6000ms'`,
		"current sessions": `ALTER ROLE "` + budgetRole + `" CONNECTION LIMIT 1`,
	}
	held := openRoleSessions(t, p, db, 2)
	defer held()
	before := roleSettings(t, p)
	for want, sql := range cases {
		_, err := e.prepareConfigChange(ctx, sql)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
	assertSuperuserRefused(t, p, e, quoted)
	if after := roleSettings(t, p); after != before {
		t.Fatalf("a refusal changed pg_db_role_setting: %q -> %q", before, after)
	}
	change, err := runRoleBudget(t, e, `ALTER ROLE "`+budgetRole+`" CONNECTION LIMIT 10`)
	if err != nil {
		t.Fatalf("limit 10 for 2 sessions: %v", err)
	}
	if change.rollbackSQL != `ALTER ROLE "`+budgetRole+`" CONNECTION LIMIT 25` {
		t.Fatalf("connection limit rollback = %q", change.rollbackSQL)
	}
	var limit int
	if err := p.QueryRow(ctx, `SELECT rolconnlimit FROM pg_roles WHERE rolname = $1`,
		budgetRole).Scan(&limit); err != nil || limit != 10 {
		t.Fatalf("rolconnlimit = %d (%v), want 10", limit, err)
	}
}

// assertSuperuserRefused makes the role a superuser for one prepare.
func assertSuperuserRefused(t *testing.T, p *pgxpool.Pool, e *Executor, quoted string) {
	t.Helper()
	ctx := context.Background()
	if _, err := p.Exec(ctx, `ALTER ROLE `+budgetRole+` SUPERUSER`); err != nil {
		t.Fatal(err)
	}
	_, err := e.prepareConfigChange(ctx, `ALTER ROLE "`+budgetRole+`" IN DATABASE `+quoted+
		` SET statement_timeout = '6000ms'`)
	if err == nil || !strings.Contains(err.Error(), "superuser") {
		t.Errorf("superuser: err = %v", err)
	}
	if _, err := p.Exec(ctx, `ALTER ROLE `+budgetRole+` NOSUPERUSER`); err != nil {
		t.Fatal(err)
	}
}

func openRoleSessions(t *testing.T, p *pgxpool.Pool, db string, n int) func() {
	t.Helper()
	var conns []*pgx.Conn
	for i := 0; i < n; i++ {
		cfg := p.Config().ConnConfig.Copy()
		cfg.User, cfg.Password, cfg.Database = budgetRole, "budget-test", db
		conn, err := pgx.ConnectConfig(context.Background(), cfg)
		if err != nil {
			t.Fatalf("connect as %s: %v", budgetRole, err)
		}
		conns = append(conns, conn)
	}
	return func() {
		for _, c := range conns {
			_ = c.Close(context.Background())
		}
	}
}

// Error propagation: a role without the right to alter another role
// (a managed provider's limited admin) gets a refusal with the
// server's message, and nothing changes.
func TestRoleBudget_PermissionErrorIsARefusal(t *testing.T) {
	p, ctx := requireDB(t)
	db := budgetRoleSetup(t, p,
		`DROP ROLE IF EXISTS budget_exec_limited`,
		`CREATE ROLE budget_exec_limited LOGIN PASSWORD 'limited-test'`)
	t.Cleanup(func() { _, _ = p.Exec(context.Background(), `DROP ROLE budget_exec_limited`) })
	cfg := p.Config().Copy()
	cfg.ConnConfig.User, cfg.ConnConfig.Password = "budget_exec_limited", "limited-test"
	limited, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Close()
	before := roleSettings(t, p)
	e := roleBudgetExecutor(limited)
	_, err = runRoleBudget(t, e, `ALTER ROLE "`+budgetRole+`" IN DATABASE `+
		pgx.Identifier{db}.Sanitize()+` SET statement_timeout = '6000ms'`)
	if !errors.Is(err, ErrRoleBudgetRefused) || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("err = %v, want a refusal carrying the permission error", err)
	}
	if after := roleSettings(t, p); after != before {
		t.Fatalf("a refused change wrote pg_db_role_setting: %q -> %q", before, after)
	}
}

// countLockHolders wraps e's role lock to count the holders at once, each
// holding it 300 ms.
func countLockHolders(e *Executor, inside, maxInside *atomic.Int32) {
	next := e.roleLock
	e.roleLock = func(ctx context.Context, role string) (func(), error) {
		release, err := next(ctx, role)
		if err != nil {
			return nil, err
		}
		n := inside.Add(1)
		for {
			m := maxInside.Load()
			if n <= m || maxInside.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
		return func() { inside.Add(-1); release() }, nil
	}
}

// Concurrency: two databases enforcing on one role serialize through
// the cluster lock: the second waits until the first releases it.
func TestRoleBudget_TwoDatabasesSerialize(t *testing.T) {
	p, ctx := requireDB(t)
	db := budgetRoleSetup(t, p)
	other, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "budgetrole2"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var otherDB string
	if err := other.QueryRow(ctx, "SELECT current_database()").Scan(&otherDB); err != nil {
		t.Fatal(err)
	}
	var inside, maxInside atomic.Int32
	e1, e2 := roleBudgetExecutor(p), roleBudgetExecutor(other)
	countLockHolders(e1, &inside, &maxInside)
	countLockHolders(e2, &inside, &maxInside)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, run := range []struct {
		e  *Executor
		db string
	}{{e1, db}, {e2, otherDB}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = runRoleBudget(t, run.e, `ALTER ROLE "`+budgetRole+`" IN DATABASE `+
				pgx.Identifier{run.db}.Sanitize()+` SET statement_timeout = '6000ms'`)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("errors = %v", errs)
	}
	if maxInside.Load() != 1 {
		t.Fatalf("%d role changes held the role lock at once, want 1", maxInside.Load())
	}
	if got := roleSettings(t, p); strings.Count(got, "statement_timeout=6000ms") != 2 {
		t.Fatalf("settings = %q, want the timeout in both databases", got)
	}
}
