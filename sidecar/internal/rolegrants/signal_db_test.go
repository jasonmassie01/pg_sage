package rolegrants_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/onboarding"
	"github.com/pg-sage/sidecar/internal/rolegrants"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: the probes are read-only catalog queries.

type signalDB struct {
	dsn     string
	admin   *pgxpool.Pool
	version int
}

func newSignalDB(t *testing.T) (signalDB, context.Context) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "rolegrants_signal")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	var v int
	if err := admin.QueryRow(ctx,
		"SELECT current_setting('server_version_num')::int").Scan(&v); err != nil {
		t.Fatalf("version: %v", err)
	}
	return signalDB{dsn: dsn, admin: admin, version: v}, ctx
}

// loginRole creates a login role (with extra role options) and connects as it.
func (d signalDB) loginRole(t *testing.T, ctx context.Context, prefix,
	opts string) (*pgxpool.Pool, string) {
	t.Helper()
	name := fmt.Sprintf("%s_%06x", prefix, time.Now().UnixNano()&0xffffff)
	rq := pgx.Identifier{name}.Sanitize()
	if _, err := d.admin.Exec(ctx, "CREATE ROLE "+rq+" LOGIN "+opts+" PASSWORD 'pw_"+
		name+"'"); err != nil {
		t.Fatalf("create role %s: %v", name, err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = d.admin.Exec(bg, "DROP OWNED BY "+rq)
		_, _ = d.admin.Exec(bg, "DROP ROLE IF EXISTS "+rq)
	})
	u, err := url.Parse(d.dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(name, "pw_"+name)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool, name
}

// memberWithoutInherit makes the role a member of pg_signal_backend that
// does not inherit its privileges: WITH INHERIT FALSE on PostgreSQL 16+,
// a NOINHERIT role before 16 (the only way there).
func (d signalDB) memberWithoutInherit(t *testing.T, ctx context.Context) (*pgxpool.Pool,
	string) {
	t.Helper()
	if d.version >= 160000 {
		pool, name := d.loginRole(t, ctx, "sig_noinh", "")
		d.exec(t, ctx, "GRANT pg_signal_backend TO "+pgx.Identifier{name}.Sanitize()+
			" WITH INHERIT FALSE")
		return pool, name
	}
	pool, name := d.loginRole(t, ctx, "sig_noinh", "NOINHERIT")
	d.exec(t, ctx, "GRANT pg_signal_backend TO "+pgx.Identifier{name}.Sanitize())
	return pool, name
}

func (d signalDB) exec(t *testing.T, ctx context.Context, sql string) {
	t.Helper()
	if _, err := d.admin.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func startupWarnings(ctx context.Context, pool *pgxpool.Pool, level string) []string {
	var out []string
	executor.VerifyGrants(ctx, pool, "ignored", level, func(_, msg string, args ...any) {
		out = append(out, fmt.Sprintf(msg, args...))
	})
	return out
}

func signalWarning(lines []string) string {
	for _, l := range lines {
		if strings.Contains(l, "pg_signal_backend") {
			return l
		}
	}
	return ""
}

func guideSignal(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (onboarding.Grant,
	onboarding.GrantStatus) {
	t.Helper()
	g, err := onboarding.CheckGrants(ctx, pool)
	if err != nil {
		t.Fatalf("CheckGrants: %v", err)
	}
	levels := onboarding.TrustGuide(onboarding.GuideInput{
		Current: onboarding.LevelObservation, Grants: g})
	for _, l := range levels {
		if l.Level != onboarding.LevelAdvisory {
			continue
		}
		for _, gr := range l.Grants {
			if gr.Name == onboarding.GrantSignalBackend {
				return gr, g
			}
		}
	}
	t.Fatal("advisory level has no pg_signal_backend grant")
	return onboarding.Grant{}, g
}

// idleBackend is the pid of an idle session of an ordinary role, one a
// pg_signal_backend member may cancel (a superuser's it may not).
func (d signalDB) idleBackend(t *testing.T, ctx context.Context) int {
	t.Helper()
	pool, _ := d.loginRole(t, ctx, "sig_victim", "")
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire victim: %v", err)
	}
	t.Cleanup(conn.Release)
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("victim pid: %v", err)
	}
	return pid
}

func cancelErr(ctx context.Context, pool *pgxpool.Pool, pid int) error {
	var ok bool
	return pool.QueryRow(ctx, "SELECT pg_cancel_backend($1)", pid).Scan(&ok)
}

// Integration: a member of pg_signal_backend that does not inherit it
// cannot cancel queries. The startup check, the Grant more guide and the
// shared probe all report it missing; the SQL they hand out fixes it, and
// then all three agree it is present and cancelling works.
func TestSignalBackendWithoutInheritIsMissing(t *testing.T) {
	d, ctx := newSignalDB(t)
	pool, name := d.memberWithoutInherit(t, ctx)
	victim := d.idleBackend(t, ctx)
	var pgErr *pgconn.PgError
	if err := cancelErr(ctx, pool, victim); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("cancel without inherit: err = %v, want permission denied (42501)", err)
	}
	sig, err := rolegrants.CheckSignalBackend(ctx, pool)
	if err != nil {
		t.Fatalf("CheckSignalBackend: %v", err)
	}
	if sig.Usable || !sig.MemberOnly {
		t.Fatalf("probe = %+v, want a member that cannot use it", sig)
	}
	g, status := guideSignal(t, ctx, pool)
	if g.Present == nil || *g.Present || status.SignalBackend == nil || *status.SignalBackend {
		t.Fatalf("guide = %+v, status = %v: want pg_signal_backend missing", g,
			status.SignalBackend)
	}
	for _, level := range []string{"advisory", "autonomous"} {
		w := signalWarning(startupWarnings(ctx, pool, level))
		if !strings.HasPrefix(w, "WARNING:") {
			t.Fatalf("%s startup: %q, want a pg_signal_backend warning", level, w)
		}
		if fix := strings.TrimSuffix(g.SQL, ";"); !strings.HasSuffix(w, fix) {
			t.Fatalf("%s: startup fix %q differs from the guide's %q", level, w, g.SQL)
		}
	}
	d.exec(t, ctx, sig.GrantSQL(pgx.Identifier{name}.Sanitize()))
	after, err := rolegrants.CheckSignalBackend(ctx, pool)
	if err != nil || !after.Usable {
		t.Fatalf("after fix: probe = %+v, err = %v, want usable", after, err)
	}
	if g, _ := guideSignal(t, ctx, pool); g.Present == nil || !*g.Present {
		t.Fatalf("after fix: guide = %+v, want present", g)
	}
	if w := signalWarning(startupWarnings(ctx, pool, "advisory")); w != "" {
		t.Fatalf("after fix: startup still warns %q", w)
	}
	if err := cancelErr(ctx, pool, victim); err != nil {
		t.Fatalf("cancel after fix: %v", err)
	}
}

// A role that is not a member at all gets the plain GRANT, which works.
func TestSignalBackendNotMember(t *testing.T) {
	d, ctx := newSignalDB(t)
	pool, name := d.loginRole(t, ctx, "sig_none", "")
	sig, err := rolegrants.CheckSignalBackend(ctx, pool)
	if err != nil {
		t.Fatalf("CheckSignalBackend: %v", err)
	}
	if sig.Usable || sig.MemberOnly {
		t.Fatalf("probe = %+v, want neither member nor usable", sig)
	}
	role := pgx.Identifier{name}.Sanitize()
	if got, want := sig.GrantSQL(role), "GRANT pg_signal_backend TO "+role; got != want {
		t.Fatalf("GrantSQL = %q, want %q", got, want)
	}
	d.exec(t, ctx, sig.GrantSQL(role))
	if after, err := rolegrants.CheckSignalBackend(ctx, pool); err != nil || !after.Usable {
		t.Fatalf("after grant: %+v, %v", after, err)
	}
}

func TestSignalBackendSuperuserIsUsable(t *testing.T) {
	d, ctx := newSignalDB(t)
	sig, err := rolegrants.CheckSignalBackend(ctx, d.admin)
	if err != nil || !sig.Usable || sig.MemberOnly {
		t.Fatalf("superuser probe = %+v, %v; want usable", sig, err)
	}
}

// Error propagation: no pool and a failed query are errors that say what
// was checked, never a guess that the grant is there or missing.
func TestSignalBackendErrors(t *testing.T) {
	if _, err := rolegrants.CheckSignalBackend(context.Background(), nil); !errors.Is(err,
		rolegrants.ErrNoPool) {
		t.Fatalf("nil pool err = %v, want ErrNoPool", err)
	}
	d, ctx := newSignalDB(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := rolegrants.CheckSignalBackend(cctx, d.admin)
	if err == nil || !strings.Contains(err.Error(), "pg_signal_backend") {
		t.Fatalf("cancelled err = %v, want one naming pg_signal_backend", err)
	}
}
