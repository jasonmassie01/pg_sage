package pgaudit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// sessionUser is the login role of pool's sessions: the user pgaudit logs.
func sessionUser(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var u string
	if err := pool.QueryRow(context.Background(), "SELECT session_user").Scan(&u); err != nil {
		t.Fatalf("session_user: %v", err)
	}
	return u
}

// nonAgentPrincipals counts stored records that name a principal although
// their database user is not an agent role (CHECK-20: must be zero).
func nonAgentPrincipals(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*)
		FROM sage.guard_pgaudit_events WHERE principal_id IS NOT NULL
		AND db_user !~ $1`, agentguard.RoleRegex).Scan(&n); err != nil {
		t.Fatalf("count non-agent principals: %v", err)
	}
	return n
}

// A session that only claims to be an agent (any role, even pg_sage's own)
// is stored unverified, naming no principal and no action; a session that
// only claims to be pg_sage is not linked to pg_sage's action even when it
// ran that action's SQL.
func TestIngestStoresSpoofedAgentAppUnverified(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_spoof")
	ctx := context.Background()
	sql := "DROP TABLE accounts"
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		executed_at) VALUES ('drop_table', $1, $2)`, sql,
		time.Date(2026, 10, 10, 12, 0, 1, 0, time.UTC)); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	own := sessionUser(t, pool)
	stats, err := NewCorrelator(pool, "orders", nil).Ingest(ctx, []logwatch.LogEntry{
		entry("pg_sage agent:ci-bot:77", "app_user", ddl),
		entry("pg_sage agent:ci-bot:78", own, ddl),
		entry("pg_sage", "mallory", `AUDIT: SESSION,1,1,DDL,DROP TABLE,TABLE,`+
			`public.accounts,`+sql),
		entry("pg_sage agent:"+idA+":79", agentguard.LoginRoleName(idA), ddl),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stats.Stored != 3 || stats.Dropped != 1 || stats.NotAudit != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if n := count(t, pool, "correlated_by = 'application_name_unverified' AND "+
		"principal_id IS NULL AND action_id IS NULL AND ((db_user = 'app_user' AND "+
		"application = 'pg_sage agent:ci-bot:77') OR (db_user = '"+own+"' AND "+
		"application = 'pg_sage agent:ci-bot:78'))"); n != 2 {
		t.Fatalf("unverified records stored = %d, want 2", n)
	}
	if n := count(t, pool, "db_user = 'mallory'"); n != 0 {
		t.Fatalf("pg_sage impersonation stored %d records", n)
	}
	if n := count(t, pool, "correlated_by = 'application_name' AND principal_id = '"+
		idA+"' AND action_id = 79 AND db_user = '"+agentguard.LoginRoleName(idA)+
		"'"); n != 1 {
		t.Fatalf("agent role record stored %d times", n)
	}
	if n := nonAgentPrincipals(t, pool); n != 0 {
		t.Fatalf("CHECK-20: %d records name a principal for a non-agent role", n)
	}
	rep, err := auditchain.Verify(ctx, pool, auditchain.PGAuditEvents, auditchain.Window{})
	if err != nil || !rep.OK() || rep.Links != 3 {
		t.Fatalf("unverified records are not chained: %+v %v", rep, err)
	}
}

// Error propagation: when pg_sage cannot read its own role, nothing is
// stored and the error says what failed.
func TestIngestFailsWhenOwnRoleUnreadable(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_role_err")
	c := NewCorrelator(pool, "orders", nil)
	pool.Close()
	_, err := c.Ingest(context.Background(), []logwatch.LogEntry{
		entry("pg_sage", "sage", ddl)})
	if err == nil || !strings.Contains(err.Error(), "own role") {
		t.Fatalf("closed pool: err = %v, want an own-role error", err)
	}
}

// Concurrent access: one correlator ingesting from several goroutines reads
// its role once and stores every record on one intact chain.
func TestIngestConcurrentOnOneCorrelator(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_concurrent")
	ctx := context.Background()
	c := NewCorrelator(pool, "orders", nil)
	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			app := fmt.Sprintf("pg_sage agent:%s:%d", idA, i+1)
			_, err := c.Ingest(ctx, []logwatch.LogEntry{
				entry(app, agentguard.BrokerRoleName(idA), ddl),
				entry(app, "app_user", ddl)})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent ingest: %v", err)
		}
	}
	if n := count(t, pool, "correlated_by = 'application_name'"); n != workers {
		t.Fatalf("attributed records = %d, want %d", n, workers)
	}
	if n := count(t, pool, "correlated_by = 'application_name_unverified'"); n != workers {
		t.Fatalf("unverified records = %d, want %d", n, workers)
	}
	if n := nonAgentPrincipals(t, pool); n != 0 {
		t.Fatalf("CHECK-20: %d records name a principal for a non-agent role", n)
	}
	rep, err := auditchain.Verify(ctx, pool, auditchain.PGAuditEvents, auditchain.Window{})
	if err != nil || !rep.OK() || rep.Links != 2*workers {
		t.Fatalf("chain after concurrent ingest: %+v %v", rep, err)
	}
}

// agentLogin creates the login agent role role, which pgaudit logs DDL
// for, and connects as it under app; the role is dropped when the test ends.
func agentLogin(t *testing.T, pool *pgxpool.Pool, dsn, role, app string) (*pgx.Conn,
	string) {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	password := hex.EncodeToString(b)
	ident := pgx.Identifier{role}.Sanitize()
	for _, s := range []string{
		"CREATE ROLE " + ident + " LOGIN PASSWORD '" + password + "'",
		"GRANT CREATE ON SCHEMA public TO " + ident,
		"ALTER ROLE " + ident + " SET pgaudit.log = 'ddl'"} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("set up agent role: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP OWNED BY "+ident)
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident)
	})
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.User, cfg.Password = role, password
	cfg.RuntimeParams["application_name"] = app
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as agent role: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	return conn, role
}
