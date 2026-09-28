package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

const rebaseKey = "analyzer.slow_query_threshold_ms"

// G5-B03: DELETE of an unrelated override used to rebuild the candidate
// from the startup config, silently re-escalating a trust level the YAML
// had since lowered. The delete must rebase on the current file config.
func TestGlobalDeleteRebasesOnCurrentFileConfig(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetRebaseConfig(t, ctx, pool)
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}

	startup := config.DefaultConfig()
	startup.Trust.Level = "autonomous"
	fileNow := config.Clone(startup)
	fileNow.Trust.Level = "observation"
	controller := config.NewConfigController(startup, nil, apiTrustOwner{})
	applyWatcherCandidate(t, ctx, pool, controller, fileNow)

	handler := rebaseRouter(pool, startup, controller, user,
		func() (*config.Config, error) { return config.Clone(fileNow), nil })
	put := serveRebase(handler, http.MethodPut, "/api/v1/config/global",
		`{"`+rebaseKey+`":"2500","expected_generation":2}`)
	if put.Code != http.StatusOK {
		t.Fatalf("put status = %d; body=%s", put.Code, put.Body.String())
	}
	// slow_query_threshold_ms is restart-bound: the override lands in the
	// desired revision, not the active one.
	if got := controller.Desired().Config.Analyzer.SlowQueryThresholdMs; got != 2500 {
		t.Fatalf("override not desired: slow_query_threshold_ms = %d", got)
	}

	del := serveRebase(handler, http.MethodDelete,
		"/api/v1/config/global/"+rebaseKey+"?expected_generation=3", "")
	if del.Code != http.StatusOK {
		t.Fatalf("delete status = %d; body=%s", del.Code, del.Body.String())
	}
	if got := controller.Active().Config.Trust.Level; got != "observation" {
		t.Fatalf("delete re-escalated live trust to %q, want observation (file value)",
			got)
	}
	desired := controller.Desired().Config
	if desired.Trust.Level != "observation" {
		t.Fatalf("desired trust = %q, want observation", desired.Trust.Level)
	}
	if desired.Analyzer.SlowQueryThresholdMs != fileNow.Analyzer.SlowQueryThresholdMs {
		t.Fatalf("deleted key = %d, want file value %d",
			desired.Analyzer.SlowQueryThresholdMs, fileNow.Analyzer.SlowQueryThresholdMs)
	}
}

// A base that cannot be loaded (e.g. the YAML is now invalid) must fail the
// delete without publishing anything.
func TestGlobalDeleteFailsClosedWhenFileConfigUnreadable(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	resetRebaseConfig(t, ctx, pool)
	user := &auth.User{ID: phase2EnsureUser(t, pool, ctx), Role: auth.RoleAdmin}
	startup := config.DefaultConfig()
	controller := config.NewConfigController(startup, nil, apiTrustOwner{})
	loadErr := errors.New("yaml: line 3: mapping values are not allowed")
	handler := rebaseRouter(pool, startup, controller, user,
		func() (*config.Config, error) { return nil, loadErr })

	put := serveRebase(handler, http.MethodPut, "/api/v1/config/global",
		`{"`+rebaseKey+`":"2500","expected_generation":1}`)
	if put.Code != http.StatusOK {
		t.Fatalf("put status = %d; body=%s", put.Code, put.Body.String())
	}
	del := serveRebase(handler, http.MethodDelete,
		"/api/v1/config/global/"+rebaseKey+"?expected_generation=2", "")
	if del.Code != http.StatusInternalServerError {
		t.Fatalf("delete status = %d, want 500; body=%s", del.Code, del.Body.String())
	}
	if got := controller.Desired().Generation; got != 2 {
		t.Fatalf("desired generation = %d, want 2 (nothing published)", got)
	}
	if got := controller.Desired().Config.Analyzer.SlowQueryThresholdMs; got != 2500 {
		t.Fatalf("override lost after failed delete: %d", got)
	}
}

func applyWatcherCandidate(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	controller *config.ConfigController, candidate *config.Config,
) {
	t.Helper()
	cs := store.NewConfigStore(pool)
	desired := controller.Desired()
	_, err := controller.ApplyWithPersistence(ctx, desired.Generation, candidate,
		func(ctx context.Context, _ config.ConfigSnapshot) error {
			_, err := cs.SetOverridesCAS(ctx, nil, 0, 0, desired.Generation)
			return err
		})
	if err != nil {
		t.Fatalf("watcher apply: %v", err)
	}
	if got := controller.Active().Config.Trust.Level; got != candidate.Trust.Level {
		t.Fatalf("watcher trust = %q, want %q", got, candidate.Trust.Level)
	}
}

func rebaseRouter(
	pool *pgxpool.Pool, startup *config.Config,
	controller *config.ConfigController, user *auth.User,
	loader func() (*config.Config, error),
) http.Handler {
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, withUser(r, user))
		})
	}
	return NewRouterFullRuntime(nil, config.Clone(startup), pool, nil, nil, nil,
		&RuntimeDeps{
			ConfigController: controller,
			ConfigBase:       startup,
			ConfigBaseLoader: loader,
		}, inject)
}

func serveRebase(
	handler http.Handler, method, path, body string,
) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func resetRebaseConfig(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	resetWave2GlobalConfig(t, ctx, pool)
	reset := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config_audit
			WHERE database_id IS NULL AND key = $1`, rebaseKey)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.config
			WHERE database_id IS NULL AND key = $1`, rebaseKey)
	}
	reset()
	t.Cleanup(reset)
}
