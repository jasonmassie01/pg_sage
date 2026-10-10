package mcpauth

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/mcpauth"))
}

func bootstrapped(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

// corePrincipals stands in for the core workstream's sage.guard_principals
// (spec §7), with the columns the resolver reads.
const corePrincipals = `CREATE TABLE sage.guard_principals (
	id text PRIMARY KEY, name text NOT NULL UNIQUE,
	status text NOT NULL DEFAULT 'active')`

// Before the core principal table exists, nothing is bound (fail closed);
// once it does, bindings resolve and retired principals do not.
func TestDBResolver(t *testing.T) {
	pool := bootstrapped(t, "mcpauth_bindings")
	ctx := context.Background()
	r := NewDBResolver(pool)
	if _, err := r.PrincipalForSubject(ctx, "https://idp", "agent-1"); !errors.Is(err,
		ErrNoBinding) {
		t.Fatalf("without the bindings table: err = %v, want ErrNoBinding", err)
	}
	mustExec(t, pool, corePrincipals)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("re-bootstrap: %v", err)
	}
	mustExec(t, pool, `INSERT INTO sage.guard_principals (id, name, status) VALUES
		($1, 'ci-bot', 'active'), ('agp_bbbbbbbbbbbbbbbbbbbb', 'old-bot', 'retired')`,
		principalA)
	mustExec(t, pool, `INSERT INTO sage.guard_identity_bindings (issuer, subject,
		principal_id, created_by) VALUES ('https://idp', 'agent-1', $1, 'test'),
		('https://idp', 'agent-old', 'agp_bbbbbbbbbbbbbbbbbbbb', 'test')`, principalA)
	got, err := r.PrincipalForSubject(ctx, "https://idp", "agent-1")
	if err != nil || got != principalA {
		t.Fatalf("bound subject = %q, %v", got, err)
	}
	for _, sub := range []string{"agent-old", "nobody"} {
		if _, err := r.PrincipalForSubject(ctx, "https://idp", sub); !errors.Is(err,
			ErrNoBinding) {
			t.Fatalf("%s: err = %v, want ErrNoBinding", sub, err)
		}
	}
	if _, err := r.PrincipalForSubject(ctx, "https://other-idp", "agent-1"); !errors.Is(err,
		ErrNoBinding) {
		t.Fatalf("same subject, other issuer: err = %v, want ErrNoBinding", err)
	}
	pool.Close()
	_, err = r.PrincipalForSubject(ctx, "https://idp", "agent-1")
	if err == nil || errors.Is(err, ErrNoBinding) {
		t.Fatalf("closed pool: err = %v, want a storage error", err)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
