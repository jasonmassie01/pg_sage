package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/advisor"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// advisors-08 on real PostgreSQL: two databases of one server, each with
// its own collector and advisor (as two fleet runtimes have), elect one
// owner from their live identity; only it asks the model the WAL question.

const fleetWALAdvice = `[{"object_identifier":"instance","severity":"warning",` +
	`"rationale":"forced checkpoints","recommended_sql":` +
	`"ALTER SYSTEM SET max_wal_size = '4GB'"}]`

// walPromptLLM is a fake model that counts WAL prompts and answers each
// with one max_wal_size recommendation.
func walPromptLLM(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var walPrompts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req llm.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode model request: %v", err)
		}
		answer := "[]"
		if len(req.Messages) > 1 && strings.Contains(req.Messages[1].Content, "max_wal_size") {
			walPrompts.Add(1)
			answer = fleetWALAdvice
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": answer},
				"finish_reason": "stop"}},
			"usage": map[string]int{"total_tokens": 10},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &walPrompts
}

func walOnlyFleetConfig(endpoint string) *config.Config {
	c := config.DefaultConfig()
	c.LLM.Enabled, c.LLM.Endpoint, c.LLM.APIKey, c.LLM.Model = true, endpoint, "k", "m"
	c.LLM.TimeoutSeconds = 10
	c.Advisor.Enabled, c.Advisor.WALEnabled = true, true
	c.Advisor.ConnectionEnabled, c.Advisor.RewriteEnabled = false, false
	c.Advisor.BloatEnabled, c.Advisor.IntervalSeconds = false, 86400
	return c
}

// fleetAdvisor is one runtime's collector and advisor on pool, joined to
// r under name. It returns once the collector has a config snapshot.
func fleetAdvisor(t *testing.T, ctx context.Context, r *instanceOwnerRegistry,
	pool *pgxpool.Pool, c *config.Config, name string) *advisor.Advisor {
	t.Helper()
	coll := collector.New(pool, c, 170000, func(string, string, ...any) {})
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go coll.Run(runCtx)
	eventually(t, 30*time.Second, name+" config snapshot", func() bool {
		s := coll.LatestSnapshot()
		return s != nil && s.ConfigData != nil && len(s.ConfigData.PGSettings) > 0
	})
	mgr := llm.NewManager(llm.New(&c.LLM, func(string, string, ...any) {}), nil, false)
	a := advisor.New(pool, c, coll, mgr, func(string, string, ...any) {})
	a.WithDatabaseName(pool.Config().ConnConfig.Database)
	m := r.join(runCtx, readInstanceKey(ctx, pool), name, true)
	a.WithInstanceOwner(r.ownerFunc(m))
	return a
}

// analyzeAndStore runs one advisor cycle and persists its findings.
func analyzeAndStore(t *testing.T, ctx context.Context, a *advisor.Advisor,
	pool *pgxpool.Pool) {
	t.Helper()
	findings, err := a.Analyze(ctx)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if err := analyzer.UpsertFindings(ctx, pool, findings); err != nil {
		t.Fatalf("persist findings: %v", err)
	}
}

// walFindings counts the open max_wal_size findings (with SQL, or advisory:
// advisors-05 keeps WAL advice advisory while the advisor window has no
// checkpoint history, as on these fresh databases) and the instance-owner
// deferrals.
func walFindings(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (executable,
	deferred int) {
	t.Helper()
	err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE concat_ws(' ', title, recommendation, recommended_sql)
			ILIKE '%max_wal_size%' AND NOT detail ? 'instance_owner'),
		count(*) FILTER (WHERE COALESCE(recommended_sql, '') = ''
			AND detail ? 'instance_owner')
		FROM sage.findings WHERE category = 'wal_tuning' AND status = 'open'`).
		Scan(&executable, &deferred)
	if err != nil {
		t.Fatalf("count wal findings: %v", err)
	}
	return executable, deferred
}

// CHECK-20: exactly one WAL prompt per interval and one max_wal_size
// finding for two databases on one server.
func TestFleetInstanceOwner_OneWALPromptForTwoDatabases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	poolA, poolB := clusterPool(t, ctx, "ownera"), clusterPool(t, ctx, "ownerb")
	url, walPrompts := walPromptLLM(t)
	c := walOnlyFleetConfig(url)
	r := newInstanceOwnerRegistry()
	beta := fleetAdvisor(t, ctx, r, poolB, c, "beta")
	alpha := fleetAdvisor(t, ctx, r, poolA, c, "alpha")
	for cycle := 1; cycle <= 2; cycle++ {
		analyzeAndStore(t, ctx, beta, poolB)
		analyzeAndStore(t, ctx, alpha, poolA)
		if n := walPrompts.Load(); n != 1 {
			t.Fatalf("cycle %d: WAL prompts = %d, want 1 per interval", cycle, n)
		}
	}
	execA, _ := walFindings(t, ctx, poolA)
	execB, deferredB := walFindings(t, ctx, poolB)
	if execA != 1 || execB != 0 || deferredB != 0 {
		t.Fatalf("max_wal_size findings: alpha=%d beta=%d (beta deferred %d), want 1/0/0",
			execA, execB, deferredB)
	}
	if !strings.Contains(strings.Join(alpha.LastEvaluatedCategories(), ","), "wal_tuning") ||
		strings.Contains(strings.Join(beta.LastEvaluatedCategories(), ","), "wal_tuning") {
		t.Fatalf("evaluated: alpha %v beta %v", alpha.LastEvaluatedCategories(),
			beta.LastEvaluatedCategories())
	}
}

// Boundary: databases on two different clusters each own their instance,
// so each asks its own WAL question.
func TestFleetInstanceOwner_TwoClustersTwoOwners(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	second := testdb.CreateDatabaseOnSecondCluster(t, "ownerc")
	poolA := clusterPool(t, ctx, "ownera2")
	poolC, err := pgxpool.New(ctx, second)
	if err != nil {
		t.Fatalf("connect second cluster: %v", err)
	}
	t.Cleanup(poolC.Close)
	if err := schema.Bootstrap(ctx, poolC); err != nil {
		t.Fatalf("bootstrap second cluster: %v", err)
	}
	url, walPrompts := walPromptLLM(t)
	c := walOnlyFleetConfig(url)
	r := newInstanceOwnerRegistry()
	alpha := fleetAdvisor(t, ctx, r, poolA, c, "alpha")
	gamma := fleetAdvisor(t, ctx, r, poolC, c, "gamma")
	analyzeAndStore(t, ctx, alpha, poolA)
	analyzeAndStore(t, ctx, gamma, poolC)
	if n := walPrompts.Load(); n != 2 {
		t.Fatalf("WAL prompts = %d, want 2 (one per cluster)", n)
	}
}

// Error propagation: a role without EXECUTE on pg_control_system() reads
// no system identifier, so the address keys the instance instead of
// failing.
func TestReadInstanceKey_PermissionDeniedFallsBack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin := clusterPool(t, ctx, "ownerperm")
	full := readInstanceKey(ctx, admin)
	if full.source != instanceKeySystemID || !strings.HasPrefix(full.key, "sysid:") ||
		strings.Contains(full.key, "standby") {
		t.Fatalf("superuser key = %+v, want the primary's system identifier", full)
	}
	restricted := restrictedRolePool(t, ctx, admin)
	cc := restricted.Config().ConnConfig
	want := factClusterAddressKey(cc.Host, cc.Port)
	if got := readInstanceKey(ctx, restricted); got.key != want ||
		got.source != instanceKeyAddress || got.reason != "" {
		t.Fatalf("restricted key = %+v, want %s", got, want)
	}
}

// restrictedRolePool connects as a login role that cannot execute
// pg_control_system(), revoking it from PUBLIC for the test if needed.
func restrictedRolePool(t *testing.T, ctx context.Context, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	role := "sage_noctl_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'noctl'`,
		role)); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	var public bool
	if err := admin.QueryRow(ctx, `SELECT has_function_privilege('public',
		'pg_catalog.pg_control_system()', 'EXECUTE')`).Scan(&public); err != nil {
		t.Fatalf("read pg_control_system privilege: %v", err)
	}
	if public {
		if _, err := admin.Exec(ctx, `REVOKE EXECUTE ON FUNCTION
			pg_catalog.pg_control_system() FROM PUBLIC`); err != nil {
			t.Fatalf("revoke pg_control_system: %v", err)
		}
		t.Cleanup(func() {
			_, _ = admin.Exec(context.Background(), `GRANT EXECUTE ON FUNCTION
				pg_catalog.pg_control_system() TO PUBLIC`)
		})
	}
	cfg := admin.Config().Copy()
	cfg.ConnConfig.User, cfg.ConnConfig.Password = role, "noctl"
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(p.Close)
	return p
}
