package agentposture

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestResolveEnv_PublicFirstSelfAndVersion(t *testing.T) {
	pool, ctx := livePool(t)
	var env Env
	var err error
	readTx(t, ctx, pool, func(tx pgx.Tx) { env, err = ResolveEnv(ctx, tx, DefaultConfig()) })
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}
	if env.VersionNum < 140000 || env.VersionNum >= 300000 {
		t.Fatalf("version = %d", env.VersionNum)
	}
	if len(env.Exposed) == 0 || env.Exposed[0].OID != PublicOID ||
		env.Exposed[0].Name != "PUBLIC" || env.Exposed[0].Source != SourcePublic {
		t.Fatalf("exposed = %+v, want PUBLIC first", env.Exposed)
	}
	var user string
	if err := pool.QueryRow(ctx, "SELECT current_user").Scan(&user); err != nil {
		t.Fatal(err)
	}
	if env.Self.Name != user || env.Self.OID == 0 {
		t.Fatalf("self = %+v, want %s", env.Self, user)
	}
	if env.Config.DailyAt != "03:00" {
		t.Fatalf("env does not carry the config: %+v", env.Config)
	}
}

func TestResolveEnv_ConfiguredExposedRolesAndMissing(t *testing.T) {
	pool, ctx := livePool(t)
	web := "web_anon_" + suffix(t)
	createRole(t, ctx, pool, web, "NOLOGIN")
	cfg := DefaultConfig()
	missing := "no_such_role_" + suffix(t)
	cfg.ExposedRoles = []string{web, missing, web}
	var env Env
	var err error
	readTx(t, ctx, pool, func(tx pgx.Tx) { env, err = ResolveEnv(ctx, tx, cfg) })
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}
	r, ok := findRole(env.Exposed, web)
	if !ok || r.Source != SourceConfigured || r.OID == 0 {
		t.Fatalf("exposed = %v, want %s configured", roleNames(env.Exposed), web)
	}
	n := 0
	for _, e := range env.Exposed {
		if e.Name == web {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%s listed %d times", web, n)
	}
	if !slices.Contains(env.Missing, missing) {
		t.Fatalf("missing = %v, want %s", env.Missing, missing)
	}
	oids := env.ExposedOIDs()
	if !slices.Contains(oids, PublicOID) || !slices.Contains(oids, r.OID) {
		t.Fatalf("ExposedOIDs = %v", oids)
	}
}

func TestResolveEnv_SupabaseRolesOnlyWhenBothExist(t *testing.T) {
	// Every creator of the cluster-wide Supabase roles holds this lock and
	// drops them before releasing it, so they never exist here.
	testdb.HoldSupabaseRoles(t, testdb.SkipUnlessLive(t))
	pool, ctx := livePool(t)
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles
		WHERE rolname IN ('anon', 'authenticated'))`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("anon or authenticated exists under the Supabase roles lock: a test " +
			"or fixture created it without testdb.HoldSupabaseRoles, or did not drop it")
	}
	createRole(t, ctx, pool, "anon", "NOLOGIN")
	resolve := func() Env {
		var env Env
		var err error
		readTx(t, ctx, pool, func(tx pgx.Tx) { env, err = ResolveEnv(ctx, tx, DefaultConfig()) })
		if err != nil {
			t.Fatalf("ResolveEnv: %v", err)
		}
		return env
	}
	if _, ok := findRole(resolve().Exposed, "anon"); ok {
		t.Fatal("anon added without authenticated")
	}
	createRole(t, ctx, pool, "authenticated", "NOLOGIN")
	env := resolve()
	for _, name := range []string{"anon", "authenticated"} {
		r, ok := findRole(env.Exposed, name)
		if !ok || r.Source != SourceSupabase {
			t.Fatalf("exposed = %+v, want %s from the Supabase rule", env.Exposed, name)
		}
	}
}

func TestResolveEnv_RegisteredAgentRolesByGuardNaming(t *testing.T) {
	pool, ctx := livePool(t)
	name := registeredRoleName(t)
	createRole(t, ctx, pool, name, "NOLOGIN")
	near := "sage_agentb_" + strings.ToUpper(suffix(t)) + "xx" // upper case: not Guard's
	createRole(t, ctx, pool, near, "NOLOGIN")
	var env Env
	var err error
	readTx(t, ctx, pool, func(tx pgx.Tx) { env, err = ResolveEnv(ctx, tx, DefaultConfig()) })
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}
	r, ok := findRole(env.Agents, name)
	if !ok || r.Source != SourceRegistered || !r.Registered() {
		t.Fatalf("agents = %v, want %s registered", roleNames(env.Agents), name)
	}
	if _, ok := findRole(env.Agents, near); ok {
		t.Fatalf("%s matched the Guard naming scheme", near)
	}
	if !env.PrincipalsExist {
		t.Fatal("a registered agent role exists, so principals exist")
	}
	if !slices.Contains(env.AgentOIDs(), r.OID) {
		t.Fatalf("AgentOIDs = %v", env.AgentOIDs())
	}
}

func TestResolveEnv_ClientHintFromApplicationName(t *testing.T) {
	pool, ctx := livePool(t)
	role := "app_" + suffix(t)
	secret := "pw-" + suffix(t)
	createRole(t, ctx, pool, role, "LOGIN PASSWORD '"+secret+"'")
	app := "ptest-mcp-" + suffix(t)
	conn := loginAs(t, ctx, role, secret, app)
	if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ClientPatterns = []string{"^PTEST-MCP-"} // case-insensitive
	var env Env
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for {
		readTx(t, ctx, pool, func(tx pgx.Tx) { env, err = ResolveEnv(ctx, tx, cfg) })
		if err != nil {
			t.Fatalf("ResolveEnv: %v", err)
		}
		if _, ok := findRole(env.Agents, role); ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	r, ok := findRole(env.Agents, role)
	if !ok || r.Source != SourceClientHint || r.Hint != app || r.Registered() {
		t.Fatalf("agents = %+v, want %s as a client hint from %s", env.Agents, role, app)
	}
	if env.PrincipalsExist && len(env.RegisteredAgents()) == 0 {
		t.Fatal("a client hint alone must not count as a principal")
	}
	for _, a := range env.HintAgents() {
		if a.Registered() {
			t.Fatalf("HintAgents returned registered role %s", a.Name)
		}
	}
	if _, ok := findRole(env.Agents, env.Self.Name); ok {
		t.Fatal("pg_sage's own role was listed as an agent")
	}
}

// A role that is registered and also connects with an agent-like client
// is listed once, as registered.
func TestResolveEnv_RegisteredWinsOverHint(t *testing.T) {
	pool, ctx := livePool(t)
	role := registeredRoleName(t)
	secret := "pw-" + suffix(t)
	createRole(t, ctx, pool, role, "LOGIN PASSWORD '"+secret+"'")
	app := "ptest-claude-" + suffix(t)
	conn := loginAs(t, ctx, role, secret, app)
	if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ClientPatterns = []string{"^ptest-claude-"}
	var env Env
	var err error
	readTx(t, ctx, pool, func(tx pgx.Tx) { env, err = ResolveEnv(ctx, tx, cfg) })
	if err != nil {
		t.Fatalf("ResolveEnv: %v", err)
	}
	n := 0
	for _, a := range env.Agents {
		if a.Name == role {
			n++
			if a.Source != SourceRegistered {
				t.Fatalf("%s listed as %s, want registered", role, a.Source)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%s listed %d times", role, n)
	}
}

func TestResolveEnv_InvalidConfigFails(t *testing.T) {
	pool, ctx := livePool(t)
	cfg := DefaultConfig()
	cfg.ClientPatterns = []string{"unanchored"}
	var err error
	readTx(t, ctx, pool, func(tx pgx.Tx) { _, err = ResolveEnv(ctx, tx, cfg) })
	if err == nil || !strings.Contains(err.Error(), "anchored") {
		t.Fatalf("ResolveEnv with an invalid config = %v", err)
	}
}

func TestResolveEnv_EndedContext(t *testing.T) {
	pool, ctx := livePool(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ResolveEnv(cctx, tx, DefaultConfig()); err == nil {
		t.Fatal("ResolveEnv succeeded on an ended context")
	}
}

// Least privilege: without pg_read_all_stats, other roles' sessions show
// a NULL backend_type in pg_stat_activity; their usesysid and
// application_name stay visible, so the hint must still be found.
func TestResolveEnv_ClientHintWithoutReadAllStats(t *testing.T) {
	pool, ctx := livePool(t)
	agent := "app_" + suffix(t)
	agentSecret := "pw-" + suffix(t)
	createRole(t, ctx, pool, agent, "LOGIN PASSWORD '"+agentSecret+"'")
	watcher := "watch_" + suffix(t)
	watchSecret := "pw-" + suffix(t)
	createRole(t, ctx, pool, watcher, "LOGIN PASSWORD '"+watchSecret+"'")
	app := "ptest-lp-mcp-" + suffix(t)
	busy := loginAs(t, ctx, agent, agentSecret, app)
	if _, err := busy.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	low := loginAs(t, ctx, watcher, watchSecret, "pg_sage")
	var stats bool
	if err := low.QueryRow(ctx, "SELECT pg_has_role('pg_read_all_stats', 'USAGE')").
		Scan(&stats); err != nil || stats {
		t.Fatalf("watcher has pg_read_all_stats=%v (%v): the test needs a role without it",
			stats, err)
	}
	cfg := DefaultConfig()
	cfg.ClientPatterns = []string{"^ptest-lp-mcp-"}
	tx, err := low.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	env, err := ResolveEnv(ctx, tx, cfg)
	if err != nil {
		t.Fatalf("ResolveEnv as a least-privilege role: %v", err)
	}
	r, ok := findRole(env.Agents, agent)
	if !ok || r.Source != SourceClientHint || r.Hint != app {
		t.Fatalf("agents = %+v, want %s hinted by %s without pg_read_all_stats",
			env.Agents, agent, app)
	}
}
