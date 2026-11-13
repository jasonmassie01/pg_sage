//go:build cgo

package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/broker"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/agentguard/readapi"
	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// security-02 end to end on real PostgreSQL: the production backend, a
// real broker and the real explainer. An agent's explain_query is
// plan-only and runs as the agent's own login role, so a table that role
// cannot read yields a permission error, not pg_sage's estimates.

type liveExplainTools struct {
	*allToolsBackend
	tools *agenttools.Tools
}

func (l liveExplainTools) ExplainQuery(ctx context.Context, r agenttools.ExplainRequest) (
	agenttools.ExplainResult, error) {
	return l.tools.ExplainQuery(ctx, r)
}

func (l liveExplainTools) ExplainAsAgent(ctx context.Context, r agenttools.ExplainRequest,
	plan agenttools.AgentPlanner) (agenttools.ExplainResult, error) {
	return l.tools.ExplainAsAgent(ctx, r, plan)
}

type liveBroker struct{ b *broker.Broker }

func (l liveBroker) AgentQuery(ctx context.Context, r readapi.Request) (readapi.Result,
	error) {
	return l.b.Query(ctx, r)
}

func (l liveBroker) AgentWhoAmI(ctx context.Context) (readapi.WhoAmI, error) {
	return l.b.WhoAmI(ctx)
}

func (l liveBroker) AgentExplain(ctx context.Context, r readapi.ExplainRequest) (
	readapi.ExplainResult, error) {
	return l.b.Explain(ctx, r)
}

// explainLogins hands the broker the test's own login role.
type explainLogins struct{ role, pw string }

func (l explainLogins) BrokerLogin(context.Context, string, string) (string, string, error) {
	return l.role, l.pw, nil
}

func (l explainLogins) BrokerRoleName(string) string { return l.role }

type oneTarget struct{ t broker.Target }

func (o oneTarget) Target(_ context.Context, name string) (broker.Target, error) {
	if name != o.t.Name {
		return broker.Target{}, broker.ErrUnknownDatabase
	}
	return o.t, nil
}

type allowReads struct{ p agentguard.Principal }

func (a allowReads) Decide(context.Context, decide.Request) decide.Verdict {
	return decide.Verdict{Allowed: true, MaxLevel: 3, Env: envbind.EnvDev,
		Capability: decide.CapRead, Principal: a.p}
}

type explainWorld struct {
	pool        *pgxpool.Pool
	schema      string
	role        string
	principalID string
	server      *Server
	agent       context.Context
}

func hexSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 5)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

func (w *explainWorld) exec(t *testing.T, sql string) {
	t.Helper()
	_, err := w.pool.Exec(context.Background(), sql)
	require.NoError(t, err, sql)
}

func newExplainWorld(t *testing.T) *explainWorld {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, schema.Bootstrap(ctx, pool))
	testdb.LockAgentRoles(t)
	suffix := hexSuffix(t)
	w := &explainWorld{pool: pool, schema: "mx_" + suffix, role: "mcp_explain_" + suffix}
	w.createObjects(t)
	p := agentguard.Principal{ID: "agp_" + suffix + suffix, Name: "explain-bot",
		Profile: "readonly-analyst", EnvCeiling: agentguard.Env("prod"),
		Status: agentguard.StatusActive, SponsorActive: true}
	b, err := broker.New(broker.DefaultConfig(), broker.Deps{
		Targets: oneTarget{broker.Target{Name: "db", Pool: pool, ClusterKey: "test",
			DatabaseID: "00000000-0000-4000-8000-0000000e" + suffix[:4]}},
		Logins:  explainLogins{role: w.role, pw: "pw-" + suffix},
		Decider: allowReads{p: p}, Classes: broker.StoreClasses{}, Audit: broker.SQLAudit{}})
	require.NoError(t, err)
	t.Cleanup(b.Close)
	tools := liveExplainTools{allToolsBackend: newAllToolsBackend(),
		tools: agenttools.New(pool, agenttools.Options{
			Explain: &config.ExplainConfig{Enabled: true}})}
	deps := newProductionFixture().dependencies()
	deps.AgentTools, deps.AgentBroker = tools, liveBroker{b: b}
	backend, err := NewProductionBackend(deps)
	require.NoError(t, err)
	w.server = NewServer(backend).WithDirectory(fleetOf("db"))
	w.principalID = p.ID
	w.agent = agentguard.WithIdentity(WithPrincipal(ctx, Principal{Actor: "token:t1",
		Kind: KindAgent, PrincipalID: p.ID, Scopes: []Scope{ScopeRead, ScopePropose},
		TokenID: "t1"}), agentguard.Identity{Principal: p, TokenID: "t1"})
	return w
}

func (w *explainWorld) createObjects(t *testing.T) {
	t.Helper()
	var db string
	require.NoError(t, w.pool.QueryRow(context.Background(),
		"SELECT current_database()").Scan(&db))
	s, r := pgx.Identifier{w.schema}.Sanitize(), pgx.Identifier{w.role}.Sanitize()
	for _, sql := range []string{"CREATE SCHEMA " + s,
		"CREATE TABLE " + s + ".t (a int PRIMARY KEY, b text NOT NULL)",
		"INSERT INTO " + s + ".t SELECT g, 'v' || g FROM generate_series(1, 2000) g",
		"CREATE TABLE " + s + ".secret_t (a int, code text)",
		"INSERT INTO " + s + ".secret_t SELECT g, 'c' || (g % 3) FROM generate_series(1, 500) g",
		"ANALYZE " + s + ".t", "ANALYZE " + s + ".secret_t",
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD 'pw-%s' NOSUPERUSER NOBYPASSRLS "+
			"CONNECTION LIMIT 2", r, strings.TrimPrefix(w.role, "mcp_explain_")),
		"GRANT CONNECT ON DATABASE " + pgx.Identifier{db}.Sanitize() + " TO " + r,
		"GRANT USAGE ON SCHEMA " + s + " TO " + r,
		"GRANT SELECT ON " + s + ".t TO " + r} {
		w.exec(t, sql)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, sql := range []string{"SELECT pg_terminate_backend(pid) FROM " +
			"pg_stat_activity WHERE usename = '" + w.role + "'",
			"DROP SCHEMA IF EXISTS " + s + " CASCADE",
			"REVOKE ALL ON DATABASE " + pgx.Identifier{db}.Sanitize() + " FROM " + r,
			"DROP ROLE IF EXISTS " + r} {
			_, _ = w.pool.Exec(ctx, sql)
		}
	})
}

func (w *explainWorld) call(t *testing.T, ctx context.Context, args string) map[string]any {
	t.Helper()
	r := invoke(t, w.server, ctx, toolCall("explain_query", args))
	require.Equal(t, 0, r.Error.Code, r.Error.Message)
	return structuredContent(t, r)
}

func TestAgentExplainAnalyzeIsPlanOnly(t *testing.T) {
	w := newExplainWorld(t)
	got := w.call(t, w.agent, `{"database":"db","query":"SELECT count(*) FROM `+w.schema+
		`.t WHERE a < 10","analyze":true}`)
	require.Equal(t, false, got["analyzed"])
	_, timed := got["actual_time_ms"]
	require.False(t, timed, "an agent got ANALYZE timings: %+v", got)
	require.Equal(t, agenttools.AgentAnalyzeRefused, got["analyze_refused"])
	require.Equal(t, "execute", got["verdict"])
	plan := fmt.Sprint(got["plan_json"])
	require.True(t, plan != "" && plan != "<nil>", "no plan: %+v", got)
	require.False(t, strings.Contains(plan, "Actual Total Time"), "plan was executed")
}

func TestAgentExplainOfAnUnreadableTableIsAPermissionError(t *testing.T) {
	w := newExplainWorld(t)
	args := `{"database":"db","query":"SELECT * FROM ` + w.schema + `.secret_t WHERE ` +
		`code = 'c1'"}`
	got := w.call(t, w.agent, args)
	require.Equal(t, "failed", got["status"], "%+v", got)
	require.Equal(t, "42501", got["sqlstate"])
	require.True(t, got["plan_json"] == nil, "the agent got estimates for secret_t: %+v", got)
	require.True(t, got["node_breakdown"] == nil || len(got["node_breakdown"].([]any)) == 0)
	// pg_sage itself can read secret_t: an operator still gets the plan.
	operator := w.call(t, WithPrincipal(context.Background(),
		Principal{Actor: "user:1", Role: "operator"}), args)
	require.True(t, operator["plan_json"] != nil, "operator plan: %+v", operator)
}

func TestAgentExplainRunsAsTheAgentsRole(t *testing.T) {
	w := newExplainWorld(t)
	w.call(t, w.agent, `{"database":"db","query":"SELECT b FROM `+w.schema+`.t WHERE a = 5"}`)
	var sessions int
	require.NoError(t, w.pool.QueryRow(context.Background(), `SELECT count(*)
		FROM pg_stat_activity WHERE usename = $1 AND datname = current_database()
		AND application_name = $2`, w.role, "pg_sage agent:"+w.principalID).Scan(&sessions))
	require.True(t, sessions >= 1, "no session of the agent's role: the explain ran elsewhere")
	if _, err := w.pool.Exec(context.Background(),
		"SELECT 1 FROM pg_stat_statements LIMIT 1"); err != nil {
		t.Skipf("pg_stat_statements unreadable, user attribution unchecked: %v", err)
	}
	count := func(role string) int {
		var n int
		require.NoError(t, w.pool.QueryRow(context.Background(), `SELECT count(*)
			FROM pg_stat_statements s JOIN pg_roles r ON r.oid = s.userid
			WHERE r.rolname = $1 AND s.query ILIKE 'EXPLAIN%' AND strpos(s.query, $2) > 0`,
			role, w.schema).Scan(&n))
		return n
	}
	var self string
	require.NoError(t, w.pool.QueryRow(context.Background(), "SELECT current_user::text").
		Scan(&self))
	require.True(t, count(w.role) >= 1, "the EXPLAIN is not recorded as the agent's role")
	require.Equal(t, 0, count(self), "the agent's EXPLAIN ran as pg_sage's role")
}

func TestOperatorExplainAnalyzeRuns(t *testing.T) {
	w := newExplainWorld(t)
	got := w.call(t, WithPrincipal(context.Background(),
		Principal{Actor: "user:1", Role: "operator"}),
		`{"database":"db","query":"SELECT count(*) FROM `+w.schema+`.t WHERE a < 10",`+
			`"analyze":true}`)
	require.Equal(t, true, got["analyzed"], "%+v", got)
	_, timed := got["actual_time_ms"]
	require.True(t, timed, "an operator's ANALYZE carries timings")
	_, verdict := got["verdict"]
	require.False(t, verdict, "pg_sage's own explainer reports no broker verdict")
}
