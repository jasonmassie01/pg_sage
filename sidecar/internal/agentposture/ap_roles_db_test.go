package agentposture

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// AP-01: agent roles with dangerous attributes or memberships.
func TestAP01_DangerousAgentRoleAttributes(t *testing.T) {
	f := newFixture(t)
	reg := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, reg, "NOLOGIN BYPASSRLS CREATEDB")
	member := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, member, "NOLOGIN")
	group := f.role("pgrp_", "NOLOGIN")
	indirect := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, indirect, "NOLOGIN")
	clean := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, clean, "NOLOGIN")
	super := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, super, "NOLOGIN SUPERUSER")
	repl := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, repl, "NOLOGIN REPLICATION")
	hint := f.role("papp_", "NOLOGIN CREATEROLE")
	f.exec("GRANT pg_read_server_files TO "+member,
		"GRANT pg_write_server_files TO "+group, "GRANT "+group+" TO "+indirect)
	env := f.env(func(e *Env) {
		for _, r := range []string{reg, member, indirect, clean, super, repl} {
			f.agent(e, r, SourceRegistered)
		}
		f.agent(e, hint, SourceClientHint)
	})
	o := f.run("AP-01", env)

	got := requireFinding(t, o, reg, Critical)
	requireContains(t, "AP-01 fix", got.FixScript, "ALTER ROLE "+reg, "NOBYPASSRLS",
		"NOCREATEDB")
	requireContains(t, "AP-01 detail", got.Detail, "BYPASSRLS", "CREATEDB")
	got = requireFinding(t, o, member, Critical)
	requireContains(t, "AP-01 fix", got.FixScript, "REVOKE pg_read_server_files FROM "+member)
	got = requireFinding(t, o, indirect, Critical)
	requireContains(t, "AP-01 detail", got.Detail, "pg_write_server_files")
	got = requireFinding(t, o, super, Critical)
	requireContains(t, "AP-01 fix", got.FixScript, "NOSUPERUSER")
	got = requireFinding(t, o, repl, Critical)
	requireContains(t, "AP-01 fix", got.FixScript, "NOREPLICATION")
	got = requireFinding(t, o, hint, Warning)
	requireContains(t, "AP-01 hint detail", got.Detail, "CREATEROLE", "hint")
	requireNoFinding(t, o, clean)
}

func TestAP01_NoAgentRolesNoFindings(t *testing.T) {
	f := newFixture(t)
	o := f.run("AP-01", f.env(func(e *Env) { e.Agents = nil }))
	if len(o.Findings) != 0 {
		t.Fatalf("findings without agent roles: %+v", o.Findings)
	}
}

// AP-02: objects owned by agent roles.
func TestAP02_ObjectsOwnedByAgentRoles(t *testing.T) {
	f := newFixture(t)
	owner := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, owner, "NOLOGIN")
	idle := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, idle, "NOLOGIN")
	hint := f.role("papp_", "NOLOGIN")
	f.exec("CREATE TABLE "+f.q("owned")+" (id int)",
		"ALTER TABLE "+f.q("owned")+" OWNER TO "+owner,
		"CREATE FUNCTION "+f.q("owned_fn")+"() RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"ALTER FUNCTION "+f.q("owned_fn")+"() OWNER TO "+owner,
		"CREATE TABLE "+f.q("hinted")+" (id int)",
		"ALTER TABLE "+f.q("hinted")+" OWNER TO "+hint)
	env := f.env(func(e *Env) {
		f.agent(e, owner, SourceRegistered)
		f.agent(e, idle, SourceRegistered)
		f.agent(e, hint, SourceClientHint)
	})
	o := f.run("AP-02", env)
	got := requireFinding(t, o, owner, Critical)
	requireContains(t, "AP-02 detail", got.Detail, "2 objects")
	requireContains(t, "AP-02 fix", got.FixScript, "REASSIGN OWNED BY "+owner)
	var refs string
	for _, e := range got.Evidence {
		refs += e.Ref + " "
	}
	requireContains(t, "AP-02 evidence", refs, f.q("owned"), "owned_fn")
	requireFinding(t, o, hint, Warning)
	requireNoFinding(t, o, idle)
}

// AP-08: agent and application login roles without timeouts.
func TestAP08_LoginRolesWithoutTimeouts(t *testing.T) {
	f := newFixture(t)
	none := f.role("plog_", "LOGIN")
	stmtOnly := f.role("plog_", "LOGIN")
	both := f.role("plog_", "LOGIN")
	bothHere := f.role("plog_", "LOGIN")
	elsewhere := f.role("plog_", "LOGIN")
	nologin := f.role("plog_", "NOLOGIN")
	super := f.role("plog_", "LOGIN SUPERUSER")
	var db string
	if err := f.pool.QueryRow(f.ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	dbq := pgx.Identifier{db}.Sanitize()
	f.exec("ALTER ROLE "+stmtOnly+" SET statement_timeout = '30s'",
		"ALTER ROLE "+both+" SET statement_timeout = '30s'",
		"ALTER ROLE "+both+" SET idle_in_transaction_session_timeout = '60s'",
		"ALTER ROLE "+bothHere+" IN DATABASE "+dbq+" SET statement_timeout = '30s'",
		"ALTER ROLE "+bothHere+" IN DATABASE "+dbq+
			" SET idle_in_transaction_session_timeout = '60s'",
		"ALTER ROLE "+elsewhere+" IN DATABASE postgres SET statement_timeout = '30s'",
		"ALTER ROLE "+elsewhere+" IN DATABASE postgres"+
			" SET idle_in_transaction_session_timeout = '60s'")
	o := f.run("AP-08", f.env(nil))
	got := requireFinding(t, o, none, Info)
	requireContains(t, "AP-08 detail", got.Detail, "statement_timeout",
		"idle_in_transaction_session_timeout")
	requireContains(t, "AP-08 fix", got.FixScript,
		"ALTER ROLE "+none+" SET statement_timeout", "idle_in_transaction_session_timeout")
	got = requireFinding(t, o, stmtOnly, Info)
	requireContains(t, "AP-08 fix", got.FixScript, "idle_in_transaction_session_timeout")
	if containsText(got.FixScript, "SET statement_timeout") {
		t.Fatalf("fix for %s sets the timeout it already has: %q", stmtOnly, got.FixScript)
	}
	requireFinding(t, o, elsewhere, Info)
	for _, r := range []string{both, bothHere, nologin, super, env0Self(t, f)} {
		requireNoFinding(t, o, r)
	}
}

// A database-wide setting covers every role of the database.
func TestAP08_DatabaseSettingCoversRoles(t *testing.T) {
	f := newFixture(t)
	role := f.role("plog_", "LOGIN")
	var db string
	if err := f.pool.QueryRow(f.ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatal(err)
	}
	dbq := pgx.Identifier{db}.Sanitize()
	f.exec("ALTER ROLE "+role+" SET statement_timeout = '30s'",
		"ALTER DATABASE "+dbq+" SET idle_in_transaction_session_timeout = '60s'")
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "ALTER DATABASE "+dbq+
			" RESET idle_in_transaction_session_timeout")
	})
	requireNoFinding(t, f.run("AP-08", f.env(nil)), role)
}

// A setting of 0 disables the timeout, so it does not count.
func TestAP08_ZeroTimeoutDoesNotCount(t *testing.T) {
	f := newFixture(t)
	role := f.role("plog_", "LOGIN")
	f.exec("ALTER ROLE "+role+" SET statement_timeout = 0",
		"ALTER ROLE "+role+" SET idle_in_transaction_session_timeout = '0'")
	got := requireFinding(t, f.run("AP-08", f.env(nil)), role, Info)
	requireContains(t, "AP-08 detail", got.Detail, "statement_timeout")
}

func env0Self(t *testing.T, f *fixture) string {
	t.Helper()
	var self string
	if err := f.pool.QueryRow(f.ctx, "SELECT current_user").Scan(&self); err != nil {
		t.Fatal(err)
	}
	return self
}

func containsText(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
