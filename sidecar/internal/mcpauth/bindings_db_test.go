package mcpauth

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
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

// Bindings resolve to principals in core's store (agentguard); a retired
// principal, an unbound subject or another issuer resolve to nothing, and a
// storage failure is not "unbound".
func TestDBResolver(t *testing.T) {
	pool := bootstrapped(t, "mcpauth_bindings")
	ctx := context.Background()
	r := NewDBResolver(pool)
	store := agentguard.NewStore(pool)
	live, err := store.Create(ctx, agentguard.CreateRequest{Name: "ci-bot",
		Profile: "readonly-analyst", CreatedBy: "test"})
	if err != nil {
		t.Fatalf("create principal: %v", err)
	}
	old, err := store.Create(ctx, agentguard.CreateRequest{Name: "old-bot",
		Profile: "readonly-analyst", CreatedBy: "test"})
	if err != nil {
		t.Fatalf("create principal: %v", err)
	}
	if _, err := store.SetStatus(ctx, old.ID, agentguard.StatusRetired, "gone"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	mustExec(t, pool, `INSERT INTO sage.guard_identity_bindings (issuer, subject,
		principal_id, created_by) VALUES ('https://idp', 'agent-1', $1, 'test'),
		('https://idp', 'agent-old', $2, 'test')`, live.ID, old.ID)
	got, err := r.PrincipalForSubject(ctx, "https://idp", "agent-1")
	if err != nil || got != live.ID {
		t.Fatalf("bound subject = %q, %v", got, err)
	}
	for _, c := range [][2]string{{"https://idp", "agent-old"}, {"https://idp", "nobody"},
		{"https://other-idp", "agent-1"}} {
		if _, err := r.PrincipalForSubject(ctx, c[0], c[1]); !errors.Is(err, ErrNoBinding) {
			t.Fatalf("%v: err = %v, want ErrNoBinding", c, err)
		}
	}
	if _, err := NewDBResolver(nil).PrincipalForSubject(ctx, "https://idp",
		"agent-1"); !errors.Is(err, ErrNoBinding) {
		t.Fatalf("no control database: err = %v", err)
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
