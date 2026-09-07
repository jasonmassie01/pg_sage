package startup

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWave5GolangCILintUsesPinnedV2Contract(t *testing.T) {
	sidecarRoot := wave5SidecarRoot(t)
	configBytes, err := os.ReadFile(filepath.Join(sidecarRoot, ".golangci.yml"))
	if err != nil {
		t.Fatalf("read golangci config: %v", err)
	}
	var config map[string]any
	if err := yaml.Unmarshal(configBytes, &config); err != nil {
		t.Fatalf("parse golangci config: %v", err)
	}
	if config["version"] != "2" {
		t.Fatalf("golangci config version = %#v, want 2", config["version"])
	}
	if _, legacy := config["linters-settings"]; legacy {
		t.Fatal("golangci config still contains v1 linters-settings")
	}

	workflowPath := filepath.Join(sidecarRoot, "..", ".github", "workflows", "test.yml")
	workflowBytes, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read lint workflow: %v", err)
	}
	workflow := string(workflowBytes)
	if !strings.Contains(workflow, "golangci/golangci-lint-action@v9") {
		t.Fatal("lint workflow does not use the v2-compatible v9 action")
	}
	if !strings.Contains(workflow, "version: v2.11.4") {
		t.Fatal("lint workflow does not pin golangci-lint v2.11.4")
	}
	if strings.Contains(workflow, "version: latest") {
		t.Fatal("lint workflow still uses an unpinned latest version")
	}
}

func TestWave5CIUsesDesignatedParallelDatabaseFixtures(t *testing.T) {
	sidecarRoot := wave5SidecarRoot(t)
	for _, name := range []string{"test.yml", "ci.yml"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(sidecarRoot, "..", ".github", "workflows", name)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read workflow: %v", err)
			}
			workflow := string(body)
			if !strings.Contains(workflow, "SAGE_TEST_DATABASE_URL:") {
				t.Fatal("workflow does not designate the test database server")
			}
			if strings.Contains(workflow, "-p 1") {
				t.Fatal("workflow still serializes packages instead of isolating fixtures")
			}
		})
	}
}

func TestWave5ContainerHealthcheckUsesPublicEndpoint(t *testing.T) {
	sidecarRoot := wave5SidecarRoot(t)
	body, err := os.ReadFile(filepath.Join(sidecarRoot, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(body)
	if !strings.Contains(dockerfile, "http://localhost:8080/health") {
		t.Fatal("container healthcheck does not use the public health endpoint")
	}
	if strings.Contains(dockerfile, "HEALTHCHECK") &&
		strings.Contains(dockerfile, "/api/v1/metrics") {
		t.Fatal("container healthcheck uses the authenticated metrics endpoint")
	}
}

func TestWave5ContainerBuildExcludesFrontendDependencies(t *testing.T) {
	sidecarRoot := wave5SidecarRoot(t)
	body, err := os.ReadFile(filepath.Join(sidecarRoot, ".dockerignore"))
	if err != nil {
		t.Fatalf("read .dockerignore: %v", err)
	}
	if !strings.Contains(string(body), "web/node_modules") {
		t.Fatal("Docker build context still includes frontend dependencies")
	}
}

func TestWave5ComposeSupportsParallelBrowserVerification(t *testing.T) {
	sidecarRoot := wave5SidecarRoot(t)
	body, err := os.ReadFile(filepath.Join(sidecarRoot, "..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatalf("read test compose file: %v", err)
	}
	compose := string(body)
	for _, contract := range []string{
		"SAGE_RATE_LIMIT: 100000",
		"PG_SAGE_DISABLE_LOGIN_RATE_LIMIT: 1",
	} {
		if !strings.Contains(compose, contract) {
			t.Errorf("test compose file omits %q", contract)
		}
	}
}

func TestWave5BrowserFixtureUsesLocalLLMMock(t *testing.T) {
	sidecarRoot := wave5SidecarRoot(t)
	body, err := os.ReadFile(filepath.Join(sidecarRoot, "..", "test-fixtures", "config.test.yaml"))
	if err != nil {
		t.Fatalf("read browser fixture config: %v", err)
	}
	config := string(body)
	if !strings.Contains(config, `endpoint: "http://llm-mock:11434/v1"`) {
		t.Fatal("browser fixture does not route LLM requests to the local mock")
	}
	if strings.Contains(config, "generativelanguage.googleapis.com") {
		t.Fatal("browser fixture still routes LLM requests to the live Gemini service")
	}
}

func TestWave5GoDiscoveryExcludesFrontendDependencies(t *testing.T) {
	sidecarRoot := wave5SidecarRoot(t)
	cmd := exec.Command("go", "list", "./...")
	cmd.Dir = sidecarRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list ./...: %v\n%s", err, output)
	}
	for _, packageName := range strings.Fields(string(output)) {
		if strings.Contains(filepath.ToSlash(packageName), "/web/node_modules/") {
			t.Fatalf("frontend dependency discovered as Go package: %s", packageName)
		}
	}
}

func wave5SidecarRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve tooling contract test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
