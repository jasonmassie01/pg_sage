package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// cli-01b (CHECK-01, CHECK-02): `config diff` and `config export` against
// the real router (session middleware, token store, config store and
// controller) on a fresh database, with overrides saved and removed the
// way Settings saves and resets them. Run on PG14 and PG17.

type configCmdDB struct {
	pool    *pgxpool.Pool
	router  http.Handler
	url     string
	file    string
	adminID int
	token   string
}

func newConfigCmdDB(t *testing.T) *configCmdDB {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "cli_config"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	d := &configCmdDB{pool: pool, file: writeTestConfig(t, "mode: standalone\n"+
		"retention:\n  snapshots_days: 30\ncollector:\n  batch_size: 400\n")}
	check, err := config.CheckFile(d.file)
	if err != nil || len(check.Problems) > 0 {
		t.Fatalf("fixture: %v %v", err, check.Problems)
	}
	mgr := fleet.NewManager(check.Config)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: pool,
		Status: &fleet.InstanceStatus{}})
	d.router, err = api.NewRouterFullRuntime(mgr, check.Config, pool, nil, nil, nil,
		&api.RuntimeDeps{ConfigController: config.NewConfigController(check.Config, nil)},
		api.SessionAuthMiddleware(pool))
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	srv := httptest.NewServer(d.router)
	t.Cleanup(srv.Close)
	d.url = srv.URL
	d.adminID, d.token = configAdminToken(t, pool, auth.RoleAdmin, []string{"*"})
	return d
}

// configAdminToken issues a read-scope operator token for a new user.
func configAdminToken(t *testing.T, pool *pgxpool.Pool, role string,
	databases []string) (int, string) {
	t.Helper()
	ctx := context.Background()
	id, err := auth.CreateUser(ctx, pool, fmt.Sprintf("cfg-%d@test.local",
		time.Now().UnixNano()), "correct horse battery", role)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	tok, err := mcptoken.NewStore(pool).Create(ctx, mcptoken.CreateRequest{
		Name: fmt.Sprintf("cfg-%d", time.Now().UnixNano()), Kind: mcptoken.KindOperator,
		Scopes: []string{mcptoken.ScopeRead}, Databases: databases, ExpiresIn: time.Hour,
		OwnerUserID: id, CreatedBy: "cfg@test.local"})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return id, tok.Secret
}

func (d *configCmdDB) run(t *testing.T, token string, args ...string) (int, string, string) {
	t.Helper()
	return runCLIEnv(t, map[string]string{"SAGE_API_URL": d.url, "SAGE_API_TOKEN": token},
		"", append([]string{configSubcommand}, args...)...)
}

// generation is the desired generation Settings sends with a write.
func (d *configCmdDB) generation(t *testing.T) uint64 {
	t.Helper()
	w := adminAPIRequest(t, d.router, d.pool, d.adminID, http.MethodGet,
		"/api/v1/config/global", nil)
	var body struct {
		Generation uint64 `json:"desired_generation"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("GET global: %d %s", w.Code, w.Body.String())
	}
	return body.Generation
}

func (d *configCmdDB) save(t *testing.T, key, value string) {
	t.Helper()
	w := adminAPIRequest(t, d.router, d.pool, d.adminID, http.MethodPut,
		"/api/v1/config/global", map[string]any{key: value,
			"expected_generation": d.generation(t)})
	if w.Code != http.StatusOK {
		t.Fatalf("save %s: %d %s", key, w.Code, w.Body.String())
	}
}

func (d *configCmdDB) reset(t *testing.T, key string) {
	t.Helper()
	w := adminAPIRequest(t, d.router, d.pool, d.adminID, http.MethodDelete,
		fmt.Sprintf("/api/v1/config/global/%s?expected_generation=%d", key, d.generation(t)),
		nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reset %s: %d %s", key, w.Code, w.Body.String())
	}
}

// State transitions: clean, then a Settings save shows in diff as a stored
// override and in export, then the reset makes diff clean again.
func TestConfigCmdDB_SaveDiffResetDiff(t *testing.T) {
	d := newConfigCmdDB(t)
	if code, out, errOut := d.run(t, d.token, "diff", d.file); code != configExitOK ||
		!strings.Contains(out, "no differences") {
		t.Fatalf("before: exit %d, out %q, err %q", code, out, errOut)
	}
	d.save(t, "retention.snapshots_days", "21")
	code, out, errOut := d.run(t, d.token, "diff", d.file)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != configExitProblem || len(lines) != 3 ||
		strings.Join(strings.Fields(lines[1]), " ") !=
			"retention.snapshots_days 30 21 stored override" {
		t.Fatalf("after save: exit %d, out %q, err %q", code, out, errOut)
	}
	code, out, errOut = d.run(t, d.token, "export", "--only-overrides")
	if code != configExitOK || out != "retention:\n    snapshots_days: 21\n" {
		t.Fatalf("export overrides: exit %d, out %q, err %q", code, out, errOut)
	}
	d.reset(t, "retention.snapshots_days")
	if code, out, errOut := d.run(t, d.token, "diff", d.file); code != configExitOK ||
		!strings.Contains(out, "no differences") {
		t.Fatalf("after reset: exit %d, out %q, err %q", code, out, errOut)
	}
	if code, out, _ := d.run(t, d.token, "export", "--only-overrides"); code !=
		configExitOK || out != "{}\n" {
		t.Fatalf("export after reset: exit %d, out %q", code, out)
	}
}

// Boundary (CHECK-02): a secret saved through Settings never comes back
// in clear, in export (both modes) or diff.
func TestConfigCmdDB_SecretNeverExported(t *testing.T) {
	d := newConfigCmdDB(t)
	d.save(t, "llm.api_key", configFixtureSecret)
	var all strings.Builder
	for _, args := range [][]string{{"export"}, {"export", "--only-overrides"},
		{"diff", d.file}} {
		code, out, errOut := d.run(t, d.token, args...)
		if code == configExitError || code == configExitAuth {
			t.Fatalf("%v: exit %d, err %q", args, code, errOut)
		}
		all.WriteString(out + errOut)
	}
	text := all.String()
	if strings.Contains(text, configFixtureSecret) || strings.Contains(text, "W8n2") {
		t.Fatalf("a secret was printed:\n%s", text)
	}
	if !strings.Contains(text, "api_key: ${SAGE_LLM_API_KEY}") ||
		!strings.Contains(text, "llm.api_key") {
		t.Fatalf("no placeholder or diff row:\n%s", text)
	}
}

// Error propagation: a non-admin token and a database-limited token are
// refused (exit 3) and print why.
func TestConfigCmdDB_NonAdminAndLimitedTokensRefused(t *testing.T) {
	d := newConfigCmdDB(t)
	_, operator := configAdminToken(t, d.pool, auth.RoleOperator, []string{"*"})
	_, limited := configAdminToken(t, d.pool, auth.RoleAdmin, []string{"orders"})
	for _, c := range []struct{ token, want string }{
		{operator, "insufficient"}, {limited, "fleet_token_required"},
	} {
		for _, args := range [][]string{{"export"}, {"diff", d.file}} {
			code, _, errOut := d.run(t, c.token, args...)
			if code != configExitAuth || !strings.Contains(errOut, c.want) {
				t.Fatalf("%v: exit %d, err %q; want 3 with %q", args, code, errOut, c.want)
			}
		}
	}
}
