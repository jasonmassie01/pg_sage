package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/store"
)

// cli-01b: GET /api/v1/config/yaml without a store (YAML fleet mode and
// the key-list paths); the stored-override path runs on PostgreSQL in
// config_yaml_handler_db_test.go. No concurrent access tests: the route
// reads one snapshot of the desired config and writes nothing.

const yamlFixtureSecret = "fixture-secret-Hq3mZ7tR"

func yamlRequest(t *testing.T, cfg *config.Config, query string) (int, map[string]any) {
	t.Helper()
	h := configYAMLHandler(nil, config.NewConfigController(cfg, nil))
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/api/v1/config/yaml"+query, nil))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: body %q: %v", query, w.Body.String(), err)
	}
	return w.Code, out
}

func secretConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.LLM.APIKey = yamlFixtureSecret
	cfg.Alerting.SlackWebhookURL = "https://hooks.example/" + yamlFixtureSecret
	cfg.Alerting.PagerDutyRoutingKey = yamlFixtureSecret
	cfg.Clone.DLEToken = yamlFixtureSecret
	cfg.Collector.BatchSize = 321
	return cfg
}

// Happy path: named keys render nested and sorted.
func TestConfigYAML_NamedKeys(t *testing.T) {
	code, out := yamlRequest(t, secretConfig(), "?keys=retention.snapshots_days,"+
		"collector.batch_size")
	want := "collector:\n    batch_size: 321\nretention:\n    snapshots_days: "
	if code != http.StatusOK || !strings.HasPrefix(out["yaml"].(string), want) {
		t.Fatalf("%d %v; want yaml starting %q", code, out, want)
	}
	keys, _ := out["keys"].([]any)
	if len(keys) != 2 || keys[0] != "collector.batch_size" || out["only_overrides"] != false {
		t.Fatalf("keys %v only_overrides %v", out["keys"], out["only_overrides"])
	}
}

// Boundary (CHECK-02): every key, every mode: the fixture secret never
// appears, its placeholder does, and the full export loads back.
func TestConfigYAML_NeverExportsASecret(t *testing.T) {
	for _, query := range []string{"", "?keys=llm.api_key,clone.dle_token",
		"?keys=alerting.slack_webhook_url,alerting.pagerduty_routing_key", "?overrides=1"} {
		code, out := yamlRequest(t, secretConfig(), query)
		raw, _ := json.Marshal(out)
		if code != http.StatusOK || strings.Contains(string(raw), yamlFixtureSecret) ||
			strings.Contains(string(raw), "Z7tR") {
			t.Fatalf("%q: %d, a secret leaked: %s", query, code, raw)
		}
	}
	_, out := yamlRequest(t, secretConfig(), "")
	text := out["yaml"].(string)
	for _, want := range []string{"api_key: ${SAGE_LLM_API_KEY}",
		"dle_token: ${SAGE_CLONE_DLE_TOKEN}", "batch_size: 321"} {
		if !strings.Contains(text, want) {
			t.Fatalf("full export lacks %q:\n%s", want, text)
		}
	}
	path := filepath.Join(t.TempDir(), "export.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	check, err := config.CheckFile(path)
	if err != nil || len(check.Problems) != 0 || check.Config.Collector.BatchSize != 321 {
		t.Fatalf("the full export does not load back: %v %v", err, check.Problems)
	}
	known := store.ConfigReadModel(secretConfig())
	if keys, _ := out["keys"].([]any); len(keys) != len(known) {
		t.Fatalf("full export has %d keys; the read model %d", len(keys), len(known))
	}
}

// Nil/empty: overrides with no store is {} (YAML fleet mode has none).
func TestConfigYAML_NoOverridesIsEmptyMap(t *testing.T) {
	code, out := yamlRequest(t, config.DefaultConfig(), "?overrides=true")
	if code != http.StatusOK || out["yaml"] != "{}\n" || out["only_overrides"] != true {
		t.Fatalf("%d %v; want {}", code, out)
	}
}

// Invalid input: an unknown key, an empty list, both modes and a bad flag
// are 400s naming the problem.
func TestConfigYAML_RefusesBadQueries(t *testing.T) {
	cases := map[string]string{
		"?keys=collector.nope":                   `unknown configuration key "collector.nope"`,
		"?keys=":                                 "names no configuration key",
		"?keys=,%20,":                            "names no configuration key",
		"?keys=collector.batch_size&overrides=1": "not both",
		"?overrides=yes":                         `overrides must be 1 or true, got "yes"`,
		"?keys=llm":                              `unknown configuration key "llm"`,
	}
	for query, want := range cases {
		code, out := yamlRequest(t, config.DefaultConfig(), query)
		msg, _ := out["error"].(string)
		if code != http.StatusBadRequest || !strings.Contains(msg, want) {
			t.Fatalf("%s: %d %v; want 400 %q", query, code, out, want)
		}
	}
}

// State transition: the route reads the controller's desired config, so
// an applied change shows at once.
func TestConfigYAML_FollowsTheDesiredConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	controller := config.NewConfigController(cfg, nil)
	next := config.Clone(cfg)
	next.Retention.SnapshotsDays = 77
	if _, err := controller.Apply(context.Background(), 1, next); err != nil {
		t.Fatalf("apply: %v", err)
	}
	h := configYAMLHandler(nil, controller)
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/api/v1/config/yaml?keys=retention.snapshots_days",
		nil))
	if !strings.Contains(w.Body.String(), `snapshots_days: 77`) {
		t.Fatalf("after apply: %s", w.Body.String())
	}
}

// The two config reads take an operator token (read scope); a token
// limited to some databases is refused: the settings are process-wide.
func TestBearerConfigReads_FleetWideOnly(t *testing.T) {
	tokens := lintTokens(map[string]mcptoken.Grant{
		lintSecret(1): operatorGrant("all", []string{mcptoken.ScopeRead}),
		lintSecret(2): operatorGrant("lim", []string{mcptoken.ScopeRead}, "orders"),
		lintSecret(3): operatorGrant("ap", []string{mcptoken.ScopeApprove}),
	})
	h, seen := cliTokenHandler(tokens)
	for _, path := range []string{"/api/v1/config/global", "/api/v1/config/yaml?keys=x"} {
		if code, _ := cliTokenCall(t, h, http.MethodGet, path, lintSecret(1),
			nil); code != http.StatusNoContent {
			t.Fatalf("%s: %d; want the route", path, code)
		}
		limited := path + "?database=orders"
		if strings.Contains(path, "?") {
			limited = path + "&database=orders"
		}
		code, out := cliTokenCall(t, h, http.MethodGet, limited, lintSecret(2), nil)
		if code != http.StatusForbidden || out["code"] != "fleet_token_required" {
			t.Fatalf("%s limited: %d %v; want 403 fleet_token_required", path, code, out)
		}
		if code, out := cliTokenCall(t, h, http.MethodGet, path, lintSecret(3),
			nil); code != http.StatusForbidden || out["code"] != "scope_missing" {
			t.Fatalf("%s no read scope: %d %v", path, code, out)
		}
	}
	if seen.calls != 2 {
		t.Fatalf("%d calls reached the routes; want 2", seen.calls)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/config/global"},
		{http.MethodDelete, "/api/v1/config/global/collector.batch_size"},
		{http.MethodGet, "/api/v1/config/audit"},
		{http.MethodGet, "/api/v1/config"},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Header.Set("Authorization", "Bearer "+lintSecret(1))
		if isCLITokenRequest(r) {
			t.Fatalf("%s %s is taken as a CLI token request", c.method, c.path)
		}
	}
}
