package pgaudit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// seedPrincipal writes a principal and its two roles on each cluster key,
// as agentguard's store does, with the given role status.
func seedPrincipal(t *testing.T, pool *pgxpool.Pool, id, name, status string,
	clusters ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.guard_principals (id, name, profile,
		created_by) VALUES ($1, $2, 'readonly-analyst', 'test')`, id, name); err != nil {
		t.Fatalf("seed principal %s: %v", name, err)
	}
	for _, k := range clusters {
		seedRoles(t, pool, id, k, agentguard.LoginRoleName(id),
			agentguard.BrokerRoleName(id), status)
	}
}

func seedRoles(t *testing.T, pool *pgxpool.Pool, id, cluster, login, brokerRole,
	status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.guard_cluster_roles
		(principal_id, cluster_key, login_role, broker_role, broker_secret_ct, key_id,
		 status) VALUES ($1, $2, $3, $4, '\x00'::bytea, 'k1', $5)`,
		id, cluster, login, brokerRole, status); err != nil {
		t.Fatalf("seed roles of %s on %s: %v", id, cluster, err)
	}
}

// Integration and state transitions: the cache reads every cluster key's
// roles with their principal's name; killed and retired principals keep
// their rows, so their earlier sessions still resolve; a role two
// principals share resolves to none.
func TestRoleCacheReadsTheControlDatabase(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_roles")
	idC := "agp_" + strings.Repeat("c", 20)
	seedPrincipal(t, pool, idA, nameA, "active", "cluster-1", "cluster-2")
	seedPrincipal(t, pool, idB, nameB, "killed", "cluster-1")
	seedPrincipal(t, pool, idC, "agent-c", "retired")
	if _, err := pool.Exec(context.Background(), `UPDATE sage.guard_principals
		SET status = 'retired' WHERE id = $1`, idB); err != nil {
		t.Fatalf("retire B: %v", err)
	}
	seedRoles(t, pool, idC, "cluster-3", agentguard.LoginRoleName(idA),
		agentguard.BrokerRoleName(idC), "retired")
	c := NewRoleCache(func() RoleQuerier { return pool }, func(err error) {
		t.Fatalf("unexpected warning: %v", err)
	})
	m, err := c.Roles(context.Background())
	if err != nil {
		t.Fatalf("read roles: %v", err)
	}
	want := map[string]Owner{
		agentguard.BrokerRoleName(idA): {ID: idA, Name: nameA},
		agentguard.LoginRoleName(idB):  {ID: idB, Name: nameB},
		agentguard.BrokerRoleName(idB): {ID: idB, Name: nameB},
		agentguard.BrokerRoleName(idC): {ID: idC, Name: "agent-c"},
	}
	if len(m) != len(want) {
		t.Fatalf("roles = %v, want %v", m, want)
	}
	for role, o := range want {
		if got, ok := m.PrincipalOfRole(role); !ok || got != o {
			t.Fatalf("role %s = %+v %v, want %+v", role, got, ok, o)
		}
	}
	if _, ok := m.PrincipalOfRole(agentguard.LoginRoleName(idA)); ok {
		t.Fatalf("a role of two principals resolved")
	}
}

// Error propagation: a missing table and a role without access are errors
// that say what was read, and warn once for the streak.
func TestRoleCacheReadErrors(t *testing.T) {
	pool, dsn := freshDB(t, "pgaudit_roles_err")
	ctx := context.Background()
	denied := deniedLogin(t, pool, dsn)
	var warns []error
	c := NewRoleCache(func() RoleQuerier { return denied }, func(err error) {
		warns = append(warns, err)
	})
	for i := 0; i < 2; i++ {
		m, err := c.Roles(ctx)
		if m != nil || err == nil || !strings.Contains(err.Error(), "permission denied") ||
			!strings.Contains(err.Error(), "guard_cluster_roles") {
			t.Fatalf("no access: %v, %v", m, err)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("no access warned %d times, want once", len(warns))
	}
	if _, err := pool.Exec(ctx,
		"ALTER TABLE sage.guard_cluster_roles RENAME TO guard_cluster_roles_gone"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	m, err := NewRoleCache(func() RoleQuerier { return pool }, nil).Roles(ctx)
	if m != nil || err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing table: %v, %v", m, err)
	}
}

// deniedLogin connects as a new login role with no access to sage.
func deniedLogin(t *testing.T, pool *pgxpool.Pool, dsn string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	role, password := "pgaudit_denied_"+hex.EncodeToString(b), hex.EncodeToString(b)
	ident := pgx.Identifier{role}.Sanitize()
	_, err := pool.Exec(ctx, "CREATE ROLE "+ident+" LOGIN PASSWORD '"+password+"'")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident) })
	if _, err := pool.Exec(ctx, "GRANT USAGE ON SCHEMA sage TO "+ident); err != nil {
		t.Fatalf("grant usage: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "REVOKE ALL ON SCHEMA sage FROM "+ident) })
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.User, cfg.Password = role, password
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	return conn
}
