package onboarding

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCheckGrantsAsAdmin(t *testing.T) {
	pool, ctx := freshInstall(t)
	if _, err := pool.Exec(ctx, "CREATE TABLE public.g_owned (id int)"); err != nil {
		t.Fatalf("table: %v", err)
	}
	g, err := CheckGrants(ctx, pool)
	if err != nil {
		t.Fatalf("check grants: %v", err)
	}
	if g.Role == "" || g.Monitor == nil || !*g.Monitor || g.SageSchema == nil ||
		!*g.SageSchema || g.TablesTotal < 1 || g.TablesOwned != g.TablesTotal {
		t.Fatalf("admin grants = %+v", g)
	}
	if g.Superuser && (g.AlterSystem == nil || !*g.AlterSystem) {
		t.Fatalf("superuser without ALTER SYSTEM: %+v", g)
	}
}

func TestCheckGrantsAsMonitorRole(t *testing.T) {
	admin, ctx := freshInstall(t)
	if _, err := admin.Exec(ctx, "CREATE TABLE public.g_admin (id int)"); err != nil {
		t.Fatalf("table: %v", err)
	}
	mon := monitorPool(t, ctx, admin)
	g, err := CheckGrants(ctx, mon)
	if err != nil {
		t.Fatalf("check grants: %v", err)
	}
	if g.Superuser || g.Monitor == nil || !*g.Monitor || g.SignalBackend == nil ||
		*g.SignalBackend || g.AlterSystem == nil || *g.AlterSystem || g.TablesOwned != 0 ||
		g.TablesTotal < 1 {
		t.Fatalf("monitor grants = %+v, want monitor only, owning no tables", g)
	}
}

func TestCheckGrantsErrors(t *testing.T) {
	if _, err := CheckGrants(context.Background(), nil); !errors.Is(err, ErrNoPool) {
		t.Fatalf("nil pool err = %v", err)
	}
}

func monitorPool(t *testing.T, ctx context.Context, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	role := fmt.Sprintf("ob_monitor_%06x", time.Now().UnixNano()&0xffffff)
	rq := pgx.Identifier{role}.Sanitize()
	var db string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("database: %v", err)
	}
	for _, s := range []string{"CREATE ROLE " + rq + " LOGIN PASSWORD 'pw_" + role + "'",
		"GRANT pg_monitor TO " + rq,
		"GRANT CONNECT ON DATABASE " + pgx.Identifier{db}.Sanitize() + " TO " + rq} {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		// DROP OWNED also revokes the CONNECT grant, which would block DROP ROLE.
		_, _ = admin.Exec(context.Background(), "DROP OWNED BY "+rq)
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+rq)
	})
	u, err := url.Parse(admin.Config().ConnString())
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(role, "pw_"+role)
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as monitor: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
