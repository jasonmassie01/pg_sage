package pgaudit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// nonIDPrincipals counts stored records whose principal_id is not an agp_
// id (CHECK-31: must be zero for rows written after audit-04b).
func nonIDPrincipals(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return count(t, pool, "principal_id IS NOT NULL AND principal_id !~ '^agp_[a-z2-7]{20}$'")
}

// principalSessions are A's and B's sessions: each lane on its own claim,
// A spoofing B by slug and B spoofing A by id, and B connected directly.
func principalSessions() []logwatch.LogEntry {
	return []logwatch.LogEntry{
		entry("pg_sage agent:"+idA+":7", agentguard.BrokerRoleName(idA), ddl),
		entry("pg_sage agent:"+nameB+":8", agentguard.LoginRoleName(idA), ddl),
		entry("pg_sage agent:"+nameA+":9", agentguard.LoginRoleName(idA), ddl),
		entry("pg_sage agent:"+idA+":10", agentguard.BrokerRoleName(idB), ddl),
		entry("psql", agentguard.BrokerRoleName(idB), ddl),
	}
}

// Integration: brokered sessions of two principals, one spoofing the other,
// give attributed rows for their own claims and unverified rows for the
// spoofs, and every principal_id is an id.
func TestIngestBindsClaimsToThePrincipalsRoles(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_principal")
	ctx := context.Background()
	seedPrincipal(t, pool, idA, nameA, "active", "cluster-1")
	seedPrincipal(t, pool, idB, nameB, "active", "cluster-1")
	c := NewCorrelator(pool, "orders", NewRoleCache(func() RoleQuerier { return pool },
		func(err error) { t.Fatalf("unexpected warning: %v", err) }))
	stats, err := c.Ingest(ctx, principalSessions())
	if err != nil || stats.Stored != 5 || stats.Dropped != 0 {
		t.Fatalf("ingest = %+v, %v", stats, err)
	}
	for _, where := range []string{
		"correlated_by = 'application_name' AND principal_id = '" + idA +
			"' AND action_id = 7",
		"correlated_by = 'application_name' AND principal_id = '" + idA +
			"' AND action_id = 9 AND application = 'pg_sage agent:" + nameA + ":9'",
		"correlated_by = 'application_name_unverified' AND principal_id IS NULL AND " +
			"action_id IS NULL AND application = 'pg_sage agent:" + nameB + ":8'",
		"correlated_by = 'application_name_unverified' AND principal_id IS NULL AND " +
			"action_id IS NULL AND application = 'pg_sage agent:" + idA + ":10'",
		"correlated_by = 'role' AND principal_id = '" + idB + "' AND action_id IS NULL",
	} {
		if n := count(t, pool, where); n != 1 {
			t.Fatalf("%d rows where %s, want 1", n, where)
		}
	}
	if n := nonIDPrincipals(t, pool); n != 0 {
		t.Fatalf("CHECK-31: %d rows name a principal by something other than its id", n)
	}
	if n := nonAgentPrincipals(t, pool); n != 0 {
		t.Fatalf("CHECK-20: %d records name a principal for a non-agent role", n)
	}
	rep, err := auditchain.Verify(ctx, pool, auditchain.PGAuditEvents, auditchain.Window{})
	if err != nil || !rep.OK() || rep.Links != 5 {
		t.Fatalf("chain: %+v %v", rep, err)
	}
}

// Error propagation: when the roles cannot be read, the batch is still
// stored whole, slug claims and direct sessions name no principal, id
// claims still verify by hash, and the failure warns once per streak.
func TestIngestWithUnreadableRolesStoresEveryRecord(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_principal_err")
	ctx := context.Background()
	seedPrincipal(t, pool, idA, nameA, "active", "cluster-1")
	seedPrincipal(t, pool, idB, nameB, "active", "cluster-1")
	if _, err := pool.Exec(ctx,
		"ALTER TABLE sage.guard_cluster_roles RENAME TO guard_cluster_roles_gone"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	warns := 0
	c := NewCorrelator(pool, "orders", NewRoleCache(func() RoleQuerier { return pool },
		func(error) { warns++ }))
	for i := 0; i < 2; i++ {
		stats, err := c.Ingest(ctx, principalSessions())
		if err != nil || stats.Stored != 5 || stats.Dropped != 0 {
			t.Fatalf("ingest %d = %+v, %v", i, stats, err)
		}
	}
	if warns != 1 {
		t.Fatalf("unreadable roles warned %d times over two batches, want 1", warns)
	}
	if n := count(t, pool, "principal_id IS NOT NULL"); n != 2 {
		t.Fatalf("%d rows name a principal, want 2 (A's own id claim, twice)", n)
	}
	if n := count(t, pool, "correlated_by = 'application_name' AND principal_id = '"+
		idA+"' AND action_id = 7"); n != 2 {
		t.Fatalf("id claims attributed %d times, want 2", n)
	}
	if n := count(t, pool, "correlated_by = 'application_name_unverified'"); n != 6 {
		t.Fatalf("unverified rows = %d, want 6", n)
	}
	if n := count(t, pool, "correlated_by = 'role' AND principal_id IS NULL"); n != 2 {
		t.Fatalf("direct sessions without a principal = %d, want 2", n)
	}
}

// Nil/empty: a correlator with no role cache verifies id claims only.
func TestIngestWithoutRoleCache(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_principal_nil")
	stats, err := NewCorrelator(pool, "orders", nil).Ingest(context.Background(),
		principalSessions())
	if err != nil || stats.Stored != 5 {
		t.Fatalf("ingest = %+v, %v", stats, err)
	}
	if n := count(t, pool, "correlated_by = 'application_name'"); n != 1 {
		t.Fatalf("attributed rows = %d, want only the id claim", n)
	}
	if n := count(t, pool, "correlated_by = 'application_name' AND principal_id = '"+
		idA+"'"); n != 1 {
		t.Fatalf("the id claim does not name A")
	}
}
