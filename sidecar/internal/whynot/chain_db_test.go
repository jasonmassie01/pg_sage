package whynot

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sqlident"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Integration: the privilege probe on a real server. The budget, stop and
// replica links come from the gate and are covered end to end by the API's
// whynot_db_test.go, where a real standing gate explains the candidates.

type probeDB struct {
	dsn   string
	admin *pgxpool.Pool
}

func newProbeDB(t *testing.T) (probeDB, context.Context) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "whynot_probe")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	return probeDB{dsn: dsn, admin: admin}, ctx
}

func (d probeDB) exec(t *testing.T, ctx context.Context, sql string) {
	t.Helper()
	if _, err := d.admin.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// loginRole creates a plain login role and connects as it.
func (d probeDB) loginRole(t *testing.T, ctx context.Context) (*pgxpool.Pool, string) {
	t.Helper()
	name := fmt.Sprintf("whynot_%06x", time.Now().UnixNano()&0xffffff)
	quoted := pgx.Identifier{name}.Sanitize()
	d.exec(t, ctx, "CREATE ROLE "+quoted+" LOGIN PASSWORD 'pw_"+name+"'")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = d.admin.Exec(bg, "DROP OWNED BY "+quoted)
		_, _ = d.admin.Exec(bg, "DROP ROLE IF EXISTS "+quoted)
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

func privilegeLinkFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool) Link {
	t.Helper()
	p, err := ProbePrivileges(ctx, pool)
	in := approvalInputs()
	in.Provider = "cloud-sql"
	in.Privileges, in.PrivilegesErr = p, err
	return linkOf(t, Chain(ctx, in), LinkPrivilege)
}

// State transition: a missing extension, then a missing grant, then the
// grant: each read reflects the catalog now, with no restart.
func TestProbePrivilegesFlipsOnTheNextRead(t *testing.T) {
	db, ctx := newProbeDB(t)
	// The fixture template installs the extension; start without it.
	db.exec(t, ctx, "DROP EXTENSION IF EXISTS pg_stat_statements")
	pool, role := db.loginRole(t, ctx)
	link := privilegeLinkFor(t, ctx, pool)
	if link.Status != Fail || link.Fix == nil ||
		!strings.Contains(link.Fix.Value, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements") ||
		!strings.Contains(link.Fix.Note, "cloudsqlsuperuser") {
		t.Fatalf("no extension: %+v %+v", link, link.Fix)
	}
	db.exec(t, ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	link = privilegeLinkFor(t, ctx, pool)
	wantGrant := "GRANT pg_read_all_stats TO " + sqlident.Role(role) + ";"
	if link.Status != Fail || link.Fix == nil || link.Fix.Value != wantGrant {
		t.Fatalf("no grant: %+v %+v, want %s", link, link.Fix, wantGrant)
	}
	db.exec(t, ctx, "GRANT pg_read_all_stats TO "+pgx.Identifier{role}.Sanitize())
	link = privilegeLinkFor(t, ctx, pool)
	if link.Status != Pass || !strings.Contains(link.Sentence, role) {
		t.Fatalf("after the grant: %+v", link)
	}
	p, err := ProbePrivileges(ctx, pool)
	if err != nil || p.Role != role || p.Replica || !p.StatStatements || !p.QueryTextVisible {
		t.Fatalf("probe = %+v, %v", p, err)
	}
}

// The probe runs read-only: a pool whose sessions refuse writes still
// answers, and a closed pool is an error with its context.
func TestProbePrivilegesReadOnlyAndErrors(t *testing.T) {
	db, ctx := newProbeDB(t)
	cfg, err := pgxpool.ParseConfig(db.dsn)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	ro, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("read-only pool: %v", err)
	}
	if p, err := ProbePrivileges(ctx, ro); err != nil || p.Role == "" {
		t.Fatalf("read-only probe = %+v, %v", p, err)
	}
	ro.Close()
	if _, err := ProbePrivileges(ctx, ro); err == nil ||
		!strings.Contains(err.Error(), "probe privileges") {
		t.Fatalf("closed pool error = %v", err)
	}
	if _, err := ProbePrivileges(ctx, nil); err == nil {
		t.Fatal("nil pool: no error")
	}
	link := privilegeLinkFor(t, ctx, ro)
	if link.Status != Unknown || link.Error == "" {
		t.Fatalf("closed pool link = %+v", link)
	}
}
