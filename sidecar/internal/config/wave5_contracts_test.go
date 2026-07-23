package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAgentNativeSurfacesDescribeIntentLevelMCP(t *testing.T) {
	root := wave5RepoRoot(t)
	paths := []string{
		filepath.Join("docs", "configuration.md"),
		filepath.Join("sidecar", "config.example.yaml"),
	}
	for _, path := range paths {
		t.Run(filepath.ToSlash(path), func(t *testing.T) {
			body := strings.ToLower(wave5ReadFile(t, filepath.Join(root, path)))
			if !strings.Contains(body, "mcp") || !strings.Contains(body, "intent") {
				t.Fatalf("agent-native surface omits intent-level MCP: %s", path)
			}
		})
	}
}

func TestWave5LegacyNotificationsNamesAlertingReplacement(t *testing.T) {
	path := wave5WriteYAML(t, "notifications:\n  slack:\n    enabled: false\n")
	err := loadYAML(path, DefaultConfig())
	if err == nil {
		t.Fatal("legacy notifications configuration was accepted")
	}
	want := `configuration key "notifications" is retired; use "alerting" instead`
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("loadYAML error = %q, want precise replacement %q", err, want)
	}
}

func TestWave5GeneratedLifecycleReferenceMatchesRegistry(t *testing.T) {
	root := wave5RepoRoot(t)
	path := filepath.Join(root, "docs", "generated", "config-lifecycles.md")
	got := wave5ReadFile(t, path)
	want := ConfigLifecycleMarkdown()
	if got != want {
		t.Fatalf(
			"generated lifecycle reference drifted; regenerate with " +
				"`go run ./cmd/gen_config_meta -lifecycle-out " +
				"../docs/generated/config-lifecycles.md`",
		)
	}
	for _, field := range FieldLifecycles() {
		if !strings.Contains(got, "| `"+field.Path+"` |") {
			t.Errorf("generated lifecycle reference omits %q", field.Path)
		}
	}
}

func TestWave5CurrentLifecycleClaimsUseTypedRegistry(t *testing.T) {
	root := wave5RepoRoot(t)
	checks := []struct {
		path  string
		stale string
		want  string
	}{
		{"sidecar/config.example.yaml", "Hot-reloadable fields",
			"docs/generated/config-lifecycles.md"},
		{"docs/reverse_spec/01-architecture.md", "HotReloadable()",
			"generated/config-lifecycles.md"},
		{"sidecar/web/src/pages/SettingsPage.jsx",
			"take effect only after a restart", "server lifecycle metadata"},
	}
	for _, check := range checks {
		body := wave5ReadFile(t, filepath.Join(root, filepath.FromSlash(check.path)))
		if strings.Contains(body, check.stale) || !strings.Contains(body, check.want) {
			t.Errorf("%s lifecycle claim is stale or omits %q", check.path, check.want)
		}
	}
}

func TestWave5AgentDBDocsDescribeImplementedAuthorityAndMonitoring(t *testing.T) {
	root := wave5RepoRoot(t)
	deployments := wave5ReadFile(
		t, filepath.Join(root, "docs", "agent-db-deployments.md"),
	)
	deployments = strings.ToLower(deployments)
	for _, claim := range []string{
		"four-layer effective policy",
		"/provision/authorize-live",
		"single-consumption",
		"durable monitoring work",
	} {
		if !strings.Contains(deployments, claim) {
			t.Errorf("AgentDB deployment guide omits current contract %q", claim)
		}
	}

	reverseSpec := wave5ReadFile(
		t, filepath.Join(root, "docs", "reverse_spec", "05-agentdb.md"),
	)
	for _, claim := range []string{
		"syncAgentDBsToFleet",
		"startAgentDBReconciler",
		"ScheduleMonitoring",
		"agent_db_live_plans",
	} {
		if !strings.Contains(reverseSpec, claim) {
			t.Errorf("AgentDB reverse spec omits current contract %q", claim)
		}
	}
	for _, stale := range []string{
		"No scheduled reconciliation",
		"No fleet/monitoring integration",
		"No goroutine/cron in `cmd/` calls these",
	} {
		if strings.Contains(reverseSpec, stale) {
			t.Errorf("AgentDB reverse spec retains stale claim %q", stale)
		}
	}
}

func wave5RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func wave5ReadFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func wave5WriteYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
